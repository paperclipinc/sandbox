package mcp

import "testing"

// TestCoreToolsSchemaValidity asserts every core tool has a non-empty name and
// description and a structurally valid object schema whose required fields all
// exist in properties.
func TestCoreToolsSchemaValidity(t *testing.T) {
	tools := coreTools()
	if len(tools) != 6 {
		t.Fatalf("expected 6 core tools, got %d", len(tools))
	}

	seen := map[string]bool{}
	for _, tool := range tools {
		if tool.Name == "" {
			t.Errorf("tool has empty name")
		}
		if seen[tool.Name] {
			t.Errorf("duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = true

		if tool.Description == "" {
			t.Errorf("tool %q has empty description", tool.Name)
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("tool %q schema type = %q, want object", tool.Name, tool.InputSchema.Type)
		}
		if tool.InputSchema.Properties == nil {
			t.Errorf("tool %q has nil properties", tool.Name)
		}
		for _, req := range tool.InputSchema.Required {
			if _, ok := tool.InputSchema.Properties[req]; !ok {
				t.Errorf("tool %q requires %q which is not in properties", tool.Name, req)
			}
		}
	}

	for _, want := range []string{
		ToolSandboxCreate, ToolSandboxExec, ToolSandboxReadFile,
		ToolSandboxWriteFile, ToolSandboxFork, ToolSandboxTerminate,
	} {
		if !seen[want] {
			t.Errorf("core tools missing %q", want)
		}
	}
}

// TestWorkspaceToolsSchemaValidity asserts the deferred workspace stubs are also
// structurally valid and match the declared name list.
func TestWorkspaceToolsSchemaValidity(t *testing.T) {
	tools := workspaceTools()
	if len(tools) != len(workspaceToolNames) {
		t.Fatalf("workspaceTools count %d != names count %d", len(tools), len(workspaceToolNames))
	}
	names := map[string]bool{}
	for _, n := range workspaceToolNames {
		names[n] = true
	}
	for _, tool := range tools {
		if !names[tool.Name] {
			t.Errorf("workspace tool %q not in workspaceToolNames", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("workspace tool %q has empty description", tool.Name)
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("workspace tool %q schema type = %q, want object", tool.Name, tool.InputSchema.Type)
		}
		for _, req := range tool.InputSchema.Required {
			if _, ok := tool.InputSchema.Properties[req]; !ok {
				t.Errorf("workspace tool %q requires %q not in properties", tool.Name, req)
			}
		}
	}
}

// TestToolSchemaVersion guards that the version constant is set.
func TestToolSchemaVersion(t *testing.T) {
	if ToolSchemaVersion == "" {
		t.Fatal("ToolSchemaVersion must not be empty")
	}
}
