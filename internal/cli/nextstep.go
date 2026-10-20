package cli

import (
	"errors"
	"fmt"

	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// withNextStep is err as it is printed: a misused command that does not say how to use it says
// where its flags are listed. Every other error says its next step where it is made (errors_test.go).
func withNextStep(err error, command string) error {
	if err == nil || view.HasNextStep(err.Error()) {
		return err
	}
	if errors.Is(err, outbox.ErrUsage) || errors.Is(err, outbox.ErrAmbiguous) {
		return fmt.Errorf("%w (`aivoicebus %s -h` lists its flags)", err, command)
	}
	return err
}
