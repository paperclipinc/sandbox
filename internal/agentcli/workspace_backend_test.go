package agentcli

import "testing"

func TestFakeWorkspaceBackendSatisfiesInterface(t *testing.T) {
	var _ WorkspaceBackend = NewFakeWorkspaceBackend()
}
