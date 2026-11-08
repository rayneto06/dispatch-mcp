package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/rayneto06/dispatch-mcp/internal/dispatch/dispatchtest"
)

// A client acts on what the server advertises: a listChanged tools
// capability makes it hold a stream open for notifications that never
// come, and a logging capability promises log messages nothing sends.
func TestCapabilitiesAdvertiseOnlyTools(t *testing.T) {
	cs, _ := newSession(t, &dispatchtest.Partner{PartnerName: "alpha"})
	got, err := json.Marshal(cs.InitializeResult().Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"tools":{}}`; string(got) != want {
		t.Errorf("capabilities = %s, want %s", got, want)
	}
}
