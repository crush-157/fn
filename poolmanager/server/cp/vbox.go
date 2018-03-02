package cp

import (
	"errors"
	"fmt"
	"log"
	"os/exec"

	"github.com/google/uuid"
)

const vagrantPrefix = "fn-vagrant"

var whichVBox *exec.Cmd

func init() {
	whichVBox = exec.Command("which", "vbox")
}

type VirtualBoxCP struct{}

func NewVirtualBoxCP() (*VirtualBoxCP, error) {
	if err := whichVBox.Run(); err != nil {
		return nil, err
	}
	return &VirtualBoxCP{}, nil
}

func (v *VirtualBoxCP) provision() error {
	name := newNodeName()
	log.Printf("This name %s", name)
	vboxProvision := exec.Command("vbox", "manage", "createvm", "--name", name, "--ostype", "Linux", "--register")
	return vboxProvision.Run()
}

func (v *VirtualBoxCP) GetLBGRunners(lgbId string) ([]*Runner, error) {
	return nil, errors.New("Not done")

}

func (v *VirtualBoxCP) ProvisionRunners(lgbId string, n int) (int, error) {
	return -1, errors.New("Not done")
}

func (v *VirtualBoxCP) RemoveRunner(lbgId string, id string) error {
	return errors.New("Not done")
}

func newNodeName() string {
	id := uuid.New()
	return fmt.Sprintf("%s-%s", vagrantPrefix, id.String())
}
