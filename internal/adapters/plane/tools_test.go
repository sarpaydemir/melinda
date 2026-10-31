package plane_test

import (
	"testing"

	"github.com/sarpaydemir/dost/internal/adapters/plane"
	"github.com/sarpaydemir/dost/internal/testkit/conformance"
)

func TestToolsKeepTheToolContract(t *testing.T) {
	conformance.Tools(t, plane.Tools(), plane.Prompts())
}
