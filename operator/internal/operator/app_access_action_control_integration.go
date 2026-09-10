//go:build integration

package operator

import (
	"context"
	"sync"
)

type IntegrationAppAccessActionEvent struct {
	Address string
	Enabled bool
}

type IntegrationAppAccessActionControl func(context.Context, IntegrationAppAccessActionEvent) error

var integrationAppAccessActionControl struct {
	sync.Mutex
	nextID  uint64
	id      uint64
	control IntegrationAppAccessActionControl
}

func InstallIntegrationAppAccessActionControl(control IntegrationAppAccessActionControl) func() {
	integrationAppAccessActionControl.Lock()
	integrationAppAccessActionControl.nextID++
	id := integrationAppAccessActionControl.nextID
	integrationAppAccessActionControl.id = id
	integrationAppAccessActionControl.control = control
	integrationAppAccessActionControl.Unlock()

	return func() {
		integrationAppAccessActionControl.Lock()
		defer integrationAppAccessActionControl.Unlock()
		if integrationAppAccessActionControl.id == id {
			integrationAppAccessActionControl.control = nil
		}
	}
}

func runAppAccessActionControl(ctx context.Context, address string, enabled bool) error {
	integrationAppAccessActionControl.Lock()
	control := integrationAppAccessActionControl.control
	integrationAppAccessActionControl.Unlock()
	if control == nil {
		return nil
	}
	return control(ctx, IntegrationAppAccessActionEvent{Address: address, Enabled: enabled})
}
