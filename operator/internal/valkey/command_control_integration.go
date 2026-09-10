//go:build integration

package valkey

import (
	"context"
	"sync"
)

type IntegrationCommandStage string

const (
	IntegrationCommandBefore IntegrationCommandStage = "before"
	IntegrationCommandAfter  IntegrationCommandStage = "after"
)

type IntegrationCommandEvent struct {
	Address string
	Name    string
	Stage   IntegrationCommandStage
}

type IntegrationCommandControl func(context.Context, IntegrationCommandEvent) error

var integrationCommandControl struct {
	sync.Mutex
	nextID  uint64
	id      uint64
	control IntegrationCommandControl
}

func InstallIntegrationCommandControl(control IntegrationCommandControl) func() {
	integrationCommandControl.Lock()
	integrationCommandControl.nextID++
	id := integrationCommandControl.nextID
	integrationCommandControl.id = id
	integrationCommandControl.control = control
	integrationCommandControl.Unlock()

	return func() {
		integrationCommandControl.Lock()
		defer integrationCommandControl.Unlock()
		if integrationCommandControl.id == id {
			integrationCommandControl.control = nil
		}
	}
}

func runCommandControl(
	ctx context.Context,
	address string,
	name string,
	stage commandControlStage,
) error {
	integrationCommandControl.Lock()
	control := integrationCommandControl.control
	integrationCommandControl.Unlock()
	if control == nil {
		return nil
	}
	integrationStage := IntegrationCommandBefore
	if stage == commandControlAfter {
		integrationStage = IntegrationCommandAfter
	}
	return control(ctx, IntegrationCommandEvent{Address: address, Name: name, Stage: integrationStage})
}
