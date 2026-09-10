//go:build !integration

package valkey

import "context"

func runCommandControl(context.Context, string, string, commandControlStage) error {
	return nil
}
