//go:build !integration

package operator

import "context"

func runAppAccessActionControl(context.Context, string, bool) error {
	return nil
}
