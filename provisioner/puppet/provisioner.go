// This package implements a provisioner for Packer that executes
// Puppet within the remote machine
package puppet

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/mitchellh/iochan"
	"github.com/mitchellh/mapstructure"
	"github.com/mitchellh/packer/packer"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

const (
	RemoteStagingPath     = "/tmp/provision/puppet"
	RemoteFileCachePath   = "/tmp/provision/puppet"
	RemoteModulePath     = "/tmp/provision/puppet/modules"
  RemoteManifestPath    = "/tmp/provision/puppet/manifest"
	DefaultModulesPath    = "modules"
)

var Ui packer.Ui

type config struct {
	// An array of local paths of modules to upload.
	ModulesPaths []string `mapstructure:"modules_paths"`

	// Option to avoid sudo use when executing commands. Defaults to false.
	PreventSudo bool `mapstructure:"prevent_sudo"`

	// If true, skips installing Puppet. Defaults to false.
	SkipInstall bool `mapstructure:"skip_install"`
}

type Provisioner struct {
	config config
}

type ExecuteRecipeTemplate struct {
	Sudo       bool
}

type ExecuteInstallPuppetTemplate struct {
	PreventSudo bool
}

func (p *Provisioner) Prepare(raws ...interface{}) error {
	errs := make([]error, 0)
	for _, raw := range raws {
		if err := mapstructure.Decode(raw, &p.config); err != nil {
			return err
		}
	}

	if p.config.ModulesPaths == nil {
		p.config.ModulesPaths = []string{DefaultModulesPath}
	}

	for _, path := range p.config.ModulesPaths {
		pFileInfo, err := os.Stat(path)

		if err != nil || !pFileInfo.IsDir() {
			errs = append(errs, fmt.Errorf("Bad module path '%s': %s", path, err))
		}
	}

	if len(errs) > 0 {
		return &packer.MultiError{errs}
	}

	return nil
}

func (p *Provisioner) Provision(ui packer.Ui, comm packer.Communicator) error {
	var err error
	Ui = ui

	if !p.config.SkipInstall {
		err = InstallPuppet(p.config.PreventSudo, comm)
		if err != nil {
			return fmt.Errorf("Error installing Puppet: %s", err)
		}
	}

	err = CreateRemoteDirectory(RemoteModulePath, comm)
	if err != nil {
		return fmt.Errorf("Error creating remote staging directory: %s", err)
	}

	// Upload all modules
	for _, path := range p.config.ModulesPaths {
		ui.Say(fmt.Sprintf("Copying module path: %s", path))
		err = UploadLocalDirectory(path, comm)
		if err != nil {
			return fmt.Errorf("Error uploading modules: %s", err)
		}
	}

	// Execute Puppet
	ui.Say("Beginning Puppet run")

	// Compile the command
	var command bytes.Buffer
	t := template.Must(template.New("puppet-run").Parse("{{if .Sudo}}sudo {{end}}puppet --verbose ???"))
	t.Execute(&command, &ExecuteRecipeTemplate{!p.config.PreventSudo})

	err = executeCommand(command.String(), comm)
	if err != nil {
		return fmt.Errorf("Error running Puppet: %s", err)
	}

	return nil
}

func UploadLocalDirectory(localDir string, comm packer.Communicator) (err error) {
	visitPath := func(path string, f os.FileInfo, err error) (err2 error) {
		var remotePath = RemoteModulePath + "/" + path
		if f.IsDir() {
			// Make remote directory
			err = CreateRemoteDirectory(remotePath, comm)
			if err != nil {
				return err
			}
		} else {
			// Upload file to existing directory
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("Error opening file: %s", err)
			}

			err = comm.Upload(remotePath, file)
			if err != nil {
				return fmt.Errorf("Error uploading file: %s", err)
			}
		}
		return
	}

	log.Printf("Uploading directory %s", localDir)
	err = filepath.Walk(localDir, visitPath)
	if err != nil {
		return fmt.Errorf("Error uploading modules %s: %s", localDir, err)
	}

	return nil
}

func CreateRemoteDirectory(path string, comm packer.Communicator) (err error) {
	log.Printf("Creating remote directory: %s ", path)

	var copyCommand = []string{"mkdir -p", path}

	var cmd packer.RemoteCmd
	cmd.Command = strings.Join(copyCommand, " ")

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	// Start the command
	if err := comm.Start(&cmd); err != nil {
		return fmt.Errorf("Unable to create remote directory %s: %d", path, err)
	}

	// Wait for it to complete
	cmd.Wait()

	return
}

func InstallPuppet(preventSudo bool, comm packer.Communicator) (err error) {
	Ui.Say("Installing Puppet")

	var command bytes.Buffer
	t := template.Must(template.New("install-puppet").Parse("{{if .sudo}}sudo {{end}}gem install puppet"))
	t.Execute(&command, map[string]bool{"sudo": !preventSudo})

	err = executeCommand(command.String(), comm)
	if err != nil {
		return fmt.Errorf("Unable to install Puppet: %d", err)
	}

	return nil
}

func executeCommand(command string, comm packer.Communicator) (err error) {
	// Setup the remote command
	stdout_r, stdout_w := io.Pipe()
	stderr_r, stderr_w := io.Pipe()

	var cmd packer.RemoteCmd
	cmd.Command = command
	cmd.Stdout = stdout_w
	cmd.Stderr = stderr_w

	log.Printf("Executing command: %s", cmd.Command)
	err = comm.Start(&cmd)
	if err != nil {
		return fmt.Errorf("Failed executing command: %s", err)
	}

	exitChan := make(chan int, 1)
	stdoutChan := iochan.DelimReader(stdout_r, '\n')
	stderrChan := iochan.DelimReader(stderr_r, '\n')

	go func() {
		defer stdout_w.Close()
		defer stderr_w.Close()

		cmd.Wait()
		exitChan <- cmd.ExitStatus
	}()

OutputLoop:
	for {
		select {
		case output := <-stderrChan:
			Ui.Message(strings.TrimSpace(output))
		case output := <-stdoutChan:
			Ui.Message(strings.TrimSpace(output))
		case exitStatus := <-exitChan:
			log.Printf("Puppet provisioner exited with status %d", exitStatus)

			if exitStatus != 0 {
				return fmt.Errorf("Command exited with non-zero exit status: %d", exitStatus)
			}

			break OutputLoop
		}
	}

	// Make sure we finish off stdout/stderr because we may have gotten
	// a message from the exit channel first.
	for output := range stdoutChan {
		Ui.Message(output)
	}

	for output := range stderrChan {
		Ui.Message(output)
	}

	return nil
}
