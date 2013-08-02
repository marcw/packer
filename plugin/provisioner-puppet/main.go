package main

import (
	"github.com/mitchellh/packer/packer/plugin"
  "github.com/jamtur01/packer/provisioner/puppet"
)

func main() {
	plugin.ServeProvisioner(new(puppet.Provisioner))
}
