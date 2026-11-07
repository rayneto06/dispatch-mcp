package mcpserver

import (
	"testing"

	"github.com/rayneto06/dispatch-mcp/internal/dispatch/dispatchtest"
)

// A client acts on what the server advertises: a listChanged tools
// capability makes it hold a stream open for notifications that never
// come, and a logging capability promises log messages nothing sends.
func TestCapabilitiesAdvertiseOnlyTools(t *testing.T) {
	cs, _ := newSession(t, &dispatchtest.Partner{PartnerName: "alpha"})
	caps := cs.InitializeResult().Capabilities
	if caps.Tools == nil {
		t.Fatal("tools capability missing")
	}
	if caps.Tools.ListChanged {
		t.Error("tools.listChanged advertised, but the tool list never changes")
	}
	if caps.Logging != nil || caps.Prompts != nil || caps.Resources != nil {
		t.Errorf("unexpected capabilities: logging=%v prompts=%v resources=%v", caps.Logging, caps.Prompts, caps.Resources)
	}
}
