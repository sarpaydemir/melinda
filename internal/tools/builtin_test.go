package tools

import (
	"testing"

	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/testkit/conformance"
)

func TestBuiltinToolsKeepTheToolContract(t *testing.T) {
	knowledge, conversation := builtin()
	conformance.Tools(t, append(knowledge, conversation...), prompt.Builtin())
}
