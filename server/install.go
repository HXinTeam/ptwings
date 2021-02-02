package server

import (
	"bufio"
	"bytes"
	"context"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/system"
)

// Executes the installation stack for a server process. Bubbles any errors up to the calling
// function which should handle contacting the panel to notify it of the server state.
//
// Pass true as the first argument in order to execute a server sync before the process to
// ensure the latest information is used.
func (s *Server) Install(sync bool) error {
	if sync {
		s.Log().Info("syncing server state with remote source before executing installation process")
		if err := s.Sync(); err != nil {
			return err
		}
	}

	var err error
	if !s.Config().SkipEggScripts {
		// Send the start event so the Panel can automatically update. We don't send this unless the process
		// is actually going to run, otherwise all sorts of weird rapid UI behavior happens since there isn't
		// an actual install process being executed.
		s.Events().Publish(InstallStartedEvent, "")

		err = s.internalInstall()
	} else {
		s.Log().Info("server configured to skip running installation scripts for this egg, not executing process")
	}

	s.Log().Debug("notifying panel of server install state")
	if serr := s.SyncInstallState(err == nil); serr != nil {
		l := s.Log().WithField("was_successful", err == nil)

		// If the request was successful but there was an error with this request, attach the
		// error to this log entry. Otherwise ignore it in this log since whatever is calling
		// this function should handle the error and will end up logging the same one.
		if err == nil {
			l.WithField("error", serr)
		}

		l.Warn("failed to notify panel of server install state")
	}

	// Ensure that the server is marked as offline at this point, otherwise you end up
	// with a blank value which is a bit confusing.
	s.Environment.SetState(environment.ProcessOfflineState)

	// Push an event to the websocket so we can auto-refresh the information in the panel once
	// the install is completed.
	s.Events().Publish(InstallCompletedEvent, "")

	return err
}

// Reinstalls a server's software by utilizing the install script for the server egg. This
// does not touch any existing files for the server, other than what the script modifies.
func (s *Server) Reinstall() error {
	if s.Environment.State() != environment.ProcessOfflineState {
		s.Log().Debug("waiting for server instance to enter a stopped state")
		if err := s.Environment.WaitForStop(10, true); err != nil {
			return err
		}
	}

	return s.Install(true)
}

// Internal installation function used to simplify reporting back to the Panel.
func (s *Server) internalInstall() error {
	script, err := s.client.GetInstallationScript(s.Context(), s.Id())
	if err != nil {
		if !remote.IsRequestError(err) {
			return err
		}

		return errors.New(err.Error())
	}

	p, err := NewInstallationProcess(s, &script)
	if err != nil {
		return err
	}

	s.Log().Info("beginning installation process for server")
	if err := p.Run(); err != nil {
		return err
	}

	s.Log().Info("completed installation process for server")
	return nil
}

type InstallationProcess struct {
	Server *Server
	Script *remote.InstallationScript

	client  *client.Client
	context context.Context
}

// Generates a new installation process struct that will be used to create containers,
// and otherwise perform installation commands for a server.
func NewInstallationProcess(s *Server, script *remote.InstallationScript) (*InstallationProcess, error) {
	proc := &InstallationProcess{
		Script: script,
		Server: s,
	}

	if c, err := environment.Docker(); err != nil {
		return nil, err
	} else {
		proc.client = c
		proc.context = s.Context()
	}

	return proc, nil
}

// Determines if the server is actively running the installation process by checking the status
// of the installer lock.
func (s *Server) IsInstalling() bool {
	return s.installing.Load()
}

func (s *Server) IsTransferring() bool {
	return s.transferring.Load()
}

func (s *Server) SetTransferring(state bool) {
	s.transferring.Store(state)
}

// Removes the installer container for the server.
func (ip *InstallationProcess) RemoveContainer() error {
	err := ip.client.ContainerRemove(ip.context, ip.Server.Id()+"_installer", types.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

// Runs the installation process, this is done as in a background thread. This will configure
// the required environment, and then spin up the installation container.
//
// Once the container finishes installing the results will be stored in an installation
// log in the server's configuration directory.
func (ip *InstallationProcess) Run() error {
	ip.Server.Log().Debug("acquiring installation process lock")
	if !ip.Server.installing.SwapIf(true) {
		return errors.New("install: cannot obtain installation lock")
	}

	// We now have an exclusive lock on this installation process. Ensure that whenever this
	// process is finished that the semaphore is released so that other processes and be executed
	// without encountering a wait timeout.
	defer func() {
		ip.Server.Log().Debug("releasing installation process lock")
		ip.Server.installing.Store(false)
	}()

	if err := ip.BeforeExecute(); err != nil {
		return err
	}

	cid, err := ip.Execute()
	if err != nil {
		ip.RemoveContainer()
		return err
	}

	// If this step fails, log a warning but don't exit out of the process. This is completely
	// internal to the daemon's functionality, and does not affect the status of the server itself.
	if err := ip.AfterExecute(cid); err != nil {
		ip.Server.Log().WithField("error", err).Warn("failed to complete after-execute step of installation process")
	}

	return nil
}

// Returns the location of the temporary data for the installation process.
func (ip *InstallationProcess) tempDir() string {
	return filepath.Join(os.TempDir(), "pterodactyl/", ip.Server.Id())
}

// Writes the installation script to a temporary file on the host machine so that it
// can be properly mounted into the installation container and then executed.
func (ip *InstallationProcess) writeScriptToDisk() error {
	// Make sure the temp directory root exists before trying to make a directory within it. The
	// ioutil.TempDir call expects this base to exist, it won't create it for you.
	if err := os.MkdirAll(ip.tempDir(), 0700); err != nil {
		return errors.WithMessage(err, "could not create temporary directory for install process")
	}

	f, err := os.OpenFile(filepath.Join(ip.tempDir(), "install.sh"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return errors.WithMessage(err, "failed to write server installation script to disk before mount")
	}
	defer f.Close()

	w := bufio.NewWriter(f)

	scanner := bufio.NewScanner(bytes.NewReader([]byte(ip.Script.Script)))
	for scanner.Scan() {
		w.WriteString(scanner.Text() + "\n")
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	w.Flush()

	return nil
}

// Pulls the docker image to be used for the installation container.
func (ip *InstallationProcess) pullInstallationImage() error {
	// Get a registry auth configuration from the config.
	var registryAuth *config.RegistryConfiguration
	for registry, c := range config.Get().Docker.Registries {
		if !strings.HasPrefix(ip.Script.ContainerImage, registry) {
			continue
		}

		log.WithField("registry", registry).Debug("using authentication for registry")
		registryAuth = &c
		break
	}

	// Get the ImagePullOptions.
	imagePullOptions := types.ImagePullOptions{All: false}
	if registryAuth != nil {
		b64, err := registryAuth.Base64()
		if err != nil {
			log.WithError(err).Error("failed to get registry auth credentials")
		}

		// b64 is a string so if there is an error it will just be empty, not nil.
		imagePullOptions.RegistryAuth = b64
	}

	r, err := ip.client.ImagePull(context.Background(), ip.Script.ContainerImage, imagePullOptions)
	if err != nil {
		images, ierr := ip.client.ImageList(context.Background(), types.ImageListOptions{})
		if ierr != nil {
			// Well damn, something has gone really wrong here, just go ahead and abort there
			// isn't much anything we can do to try and self-recover from this.
			return ierr
		}

		for _, img := range images {
			for _, t := range img.RepoTags {
				if t != ip.Script.ContainerImage {
					continue
				}

				log.WithFields(log.Fields{
					"image": ip.Script.ContainerImage,
					"err":   err.Error(),
				}).Warn("unable to pull requested image from remote source, however the image exists locally")

				// Okay, we found a matching container image, in that case just go ahead and return
				// from this function, since there is nothing else we need to do here.
				return nil
			}
		}

		return err
	}
	defer r.Close()

	log.WithField("image", ip.Script.ContainerImage).Debug("pulling docker image... this could take a bit of time")

	// Block continuation until the image has been pulled successfully.
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		log.Debug(scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

// Runs before the container is executed. This pulls down the required docker container image
// as well as writes the installation script to the disk. This process is executed in an async
// manner, if either one fails the error is returned.
func (ip *InstallationProcess) BeforeExecute() error {
	if err := ip.writeScriptToDisk(); err != nil {
		return errors.WithMessage(err, "failed to write installation script to disk")
	}
	if err := ip.pullInstallationImage(); err != nil {
		return errors.WithMessage(err, "failed to pull updated installation container image for server")
	}
	if err := ip.RemoveContainer(); err != nil {
		return errors.WithMessage(err, "failed to remove existing install container for server")
	}
	return nil
}

// Returns the log path for the installation process.
func (ip *InstallationProcess) GetLogPath() string {
	return filepath.Join(config.Get().System.LogDirectory, "/install", ip.Server.Id()+".log")
}

// Cleans up after the execution of the installation process. This grabs the logs from the
// process to store in the server configuration directory, and then destroys the associated
// installation container.
func (ip *InstallationProcess) AfterExecute(containerId string) error {
	defer ip.RemoveContainer()

	ip.Server.Log().WithField("container_id", containerId).Debug("pulling installation logs for server")
	reader, err := ip.client.ContainerLogs(ip.context, containerId, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     false,
	})

	if err != nil && !client.IsErrNotFound(err) {
		return err
	}

	f, err := os.OpenFile(ip.GetLogPath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	// We write the contents of the container output to a more "permanent" file so that they
	// can be referenced after this container is deleted. We'll also include the environment
	// variables passed into the container to make debugging things a little easier.
	ip.Server.Log().WithField("path", ip.GetLogPath()).Debug("writing most recent installation logs to disk")

	tmpl, err := template.New("header").Parse(`Pterodactyl Server Installation Log

|
| Details
| ------------------------------
  Server UUID:          {{.Server.Id}}
  Container Image:      {{.Script.ContainerImage}}
  Container Entrypoint: {{.Script.Entrypoint}}

|
| Environment Variables
| ------------------------------
{{ range $key, $value := .Server.GetEnvironmentVariables }}  {{ $value }}
{{ end }}

|
| Script Output
| ------------------------------
`)
	if err != nil {
		return err
	}

	if err := tmpl.Execute(f, ip); err != nil {
		return err
	}

	if _, err := io.Copy(f, reader); err != nil {
		return err
	}

	return nil
}

// Executes the installation process inside a specially created docker container.
func (ip *InstallationProcess) Execute() (string, error) {
	// Create a child context that is canceled once this function is done running. This
	// will also be canceled if the parent context (from the Server struct) is canceled
	// which occurs if the server is deleted.
	ctx, cancel := context.WithCancel(ip.context)
	defer cancel()

	conf := &container.Config{
		Hostname:     "installer",
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  true,
		OpenStdin:    true,
		Tty:          true,
		Cmd:          []string{ip.Script.Entrypoint, "/mnt/install/install.sh"},
		Image:        ip.Script.ContainerImage,
		Env:          ip.Server.GetEnvironmentVariables(),
		Labels: map[string]string{
			"Service":       "Pterodactyl",
			"ContainerType": "server_installer",
		},
	}

	tmpfsSize := strconv.Itoa(int(config.Get().Docker.TmpfsSize))
	hostConf := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Target:   "/mnt/server",
				Source:   ip.Server.Filesystem().Path(),
				Type:     mount.TypeBind,
				ReadOnly: false,
			},
			{
				Target:   "/mnt/install",
				Source:   ip.tempDir(),
				Type:     mount.TypeBind,
				ReadOnly: false,
			},
		},
		Tmpfs: map[string]string{
			"/tmp": "rw,exec,nosuid,size=" + tmpfsSize + "M",
		},
		DNS: config.Get().Docker.Network.Dns,
		LogConfig: container.LogConfig{
			Type: "local",
			Config: map[string]string{
				"max-size": "5m",
				"max-file": "1",
				"compress": "false",
			},
		},
		Privileged:  true,
		NetworkMode: container.NetworkMode(config.Get().Docker.Network.Mode),
	}

	// Ensure the root directory for the server exists properly before attempting
	// to trigger the reinstall of the server. It is possible the directory would
	// not exist when this runs if Wings boots with a missing directory and a user
	// triggers a reinstall before trying to start the server.
	if err := ip.Server.EnsureDataDirectoryExists(); err != nil {
		return "", err
	}

	ip.Server.Log().WithField("install_script", ip.tempDir()+"/install.sh").Info("creating install container for server process")
	// Remove the temporary directory when the installation process finishes for this server container.
	defer func() {
		if err := os.RemoveAll(ip.tempDir()); err != nil {
			if !os.IsNotExist(err) {
				ip.Server.Log().WithField("error", err).Warn("failed to remove temporary data directory after install process")
			}
		}
	}()

	r, err := ip.client.ContainerCreate(ctx, conf, hostConf, nil, nil, ip.Server.Id()+"_installer")
	if err != nil {
		return "", err
	}

	ip.Server.Log().WithField("container_id", r.ID).Info("running installation script for server in container")
	if err := ip.client.ContainerStart(ctx, r.ID, types.ContainerStartOptions{}); err != nil {
		return "", err
	}

	// Process the install event in the background by listening to the stream output until the
	// container has stopped, at which point we'll disconnect from it.
	//
	// If there is an error during the streaming output just report it and do nothing else, the
	// install can still run, the console just won't have any output.
	go func(id string) {
		ip.Server.Events().Publish(DaemonMessageEvent, "Starting installation process, this could take a few minutes...")
		if err := ip.StreamOutput(ctx, id); err != nil {
			ip.Server.Log().WithField("error", err).Warn("error connecting to server install stream output")
		}
	}(r.ID)

	sChan, eChan := ip.client.ContainerWait(ctx, r.ID, container.WaitConditionNotRunning)
	select {
	case err := <-eChan:
		// Once the container has stopped running we can mark the install process as being completed.
		if err == nil {
			ip.Server.Events().Publish(DaemonMessageEvent, "Installation process completed.")
		} else {
			return "", err
		}
	case <-sChan:
	}

	return r.ID, nil
}

// Streams the output of the installation process to a log file in the server configuration
// directory, as well as to a websocket listener so that the process can be viewed in
// the panel by administrators.
func (ip *InstallationProcess) StreamOutput(ctx context.Context, id string) error {
	reader, err := ip.client.ContainerLogs(ctx, id, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})

	if err != nil {
		return err
	}
	defer reader.Close()

	evts := ip.Server.Events()
	err = system.ScanReader(reader, func(line string) {
		evts.Publish(InstallOutputEvent, line)
	})
	if err != nil {
		ip.Server.Log().WithFields(log.Fields{"container_id": id, "error": err}).Warn("error processing install output lines")
	}
	return nil
}

// Makes a HTTP request to the Panel instance notifying it that the server has
// completed the installation process, and what the state of the server is. A boolean
// value of "true" means everything was successful, "false" means something went
// wrong and the server must be deleted and re-created.
func (s *Server) SyncInstallState(successful bool) error {
	err := s.client.SetInstallationStatus(s.Context(), s.Id(), successful)
	if err != nil {
		if !remote.IsRequestError(err) {
			return err
		}

		return errors.New(err.Error())
	}

	return nil
}
