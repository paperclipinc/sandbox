package mcp

// ToolSchemaVersion identifies the version of the tool input-schema contract
// this server advertises. Bump it whenever a tool's name, required fields, or
// property semantics change in a way clients must observe.
const ToolSchemaVersion = "1.0.0"

// JSONSchema is the subset of JSON Schema used for tool input schemas: an
// object with named properties and a required list.
type JSONSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]SchemaProperty `json:"properties"`
	Required   []string                  `json:"required,omitempty"`
}

// SchemaProperty describes a single property in a tool input schema.
type SchemaProperty struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// Tool is an MCP tool advertised over tools/list and dispatched via tools/call.
type Tool struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	InputSchema JSONSchema `json:"inputSchema"`
}

// Tool names. Centralized so the server dispatch and tests share one source of
// truth.
const (
	ToolSandboxCreate    = "sandbox_create"
	ToolSandboxExec      = "sandbox_exec"
	ToolSandboxReadFile  = "sandbox_read_file"
	ToolSandboxWriteFile = "sandbox_write_file"
	ToolSandboxFork      = "sandbox_fork"
	ToolSandboxTerminate = "sandbox_terminate"
)

func str(desc string) SchemaProperty { return SchemaProperty{Type: "string", Description: desc} }
func num(desc string) SchemaProperty { return SchemaProperty{Type: "integer", Description: desc} }

// coreTools returns the six always-on sandbox lifecycle tools.
func coreTools() []Tool {
	return []Tool{
		{
			Name:        ToolSandboxCreate,
			Description: "Create a new sandbox from a sandbox pool and return its id.",
			InputSchema: JSONSchema{
				Type: "object",
				Properties: map[string]SchemaProperty{
					"pool": str("Name of the SandboxPool to create the sandbox from."),
				},
				Required: []string{"pool"},
			},
		},
		{
			Name:        ToolSandboxExec,
			Description: "Run a shell command inside a sandbox and return its exit code, stdout, and stderr.",
			InputSchema: JSONSchema{
				Type: "object",
				Properties: map[string]SchemaProperty{
					"sandbox":         str("Id of the sandbox to run the command in."),
					"command":         str("Shell command to execute."),
					"timeout_seconds": num("Optional timeout in seconds; 0 or omitted uses the backend default."),
				},
				Required: []string{"sandbox", "command"},
			},
		},
		{
			Name:        ToolSandboxReadFile,
			Description: "Read the contents of a file inside a sandbox.",
			InputSchema: JSONSchema{
				Type: "object",
				Properties: map[string]SchemaProperty{
					"sandbox": str("Id of the sandbox to read from."),
					"path":    str("Absolute path of the file to read inside the sandbox."),
				},
				Required: []string{"sandbox", "path"},
			},
		},
		{
			Name:        ToolSandboxWriteFile,
			Description: "Write contents to a file inside a sandbox, creating or overwriting it.",
			InputSchema: JSONSchema{
				Type: "object",
				Properties: map[string]SchemaProperty{
					"sandbox": str("Id of the sandbox to write to."),
					"path":    str("Absolute path of the file to write inside the sandbox."),
					"content": str("Contents to write to the file."),
				},
				Required: []string{"sandbox", "path", "content"},
			},
		},
		{
			Name:        ToolSandboxFork,
			Description: "Fork a sandbox into one or more copies and return the new sandbox ids.",
			InputSchema: JSONSchema{
				Type: "object",
				Properties: map[string]SchemaProperty{
					"sandbox":  str("Id of the sandbox to fork."),
					"replicas": num("Optional number of forks to create; omitted means one."),
				},
				Required: []string{"sandbox"},
			},
		},
		{
			Name:        ToolSandboxTerminate,
			Description: "Terminate a sandbox and release its resources.",
			InputSchema: JSONSchema{
				Type: "object",
				Properties: map[string]SchemaProperty{
					"sandbox": str("Id of the sandbox to terminate."),
				},
				Required: []string{"sandbox"},
			},
		},
	}
}

// workspaceToolNames lists the workspace tools that will be registered once
// workspace support (#21) lands. They are advertised only when
// Options.EnableWorkspaceTools is set; dispatch is deferred and intentionally
// not yet wired to the backend.
var workspaceToolNames = []string{
	"workspace_create",
	"workspace_list",
	"workspace_attach",
	"workspace_delete",
}

// workspaceTools returns stub Tool definitions for the workspace tools. These
// carry valid schemas so clients can introspect them, but the server does not
// dispatch them yet.
func workspaceTools() []Tool {
	return []Tool{
		{
			Name:        "workspace_create",
			Description: "Create a persistent workspace volume. Deferred: not yet dispatched (issue #21).",
			InputSchema: JSONSchema{
				Type:       "object",
				Properties: map[string]SchemaProperty{"name": str("Name of the workspace to create.")},
				Required:   []string{"name"},
			},
		},
		{
			Name:        "workspace_list",
			Description: "List persistent workspaces. Deferred: not yet dispatched (issue #21).",
			InputSchema: JSONSchema{
				Type:       "object",
				Properties: map[string]SchemaProperty{},
			},
		},
		{
			Name:        "workspace_attach",
			Description: "Attach a workspace to a sandbox. Deferred: not yet dispatched (issue #21).",
			InputSchema: JSONSchema{
				Type: "object",
				Properties: map[string]SchemaProperty{
					"sandbox":   str("Id of the sandbox to attach to."),
					"workspace": str("Name of the workspace to attach."),
				},
				Required: []string{"sandbox", "workspace"},
			},
		},
		{
			Name:        "workspace_delete",
			Description: "Delete a persistent workspace. Deferred: not yet dispatched (issue #21).",
			InputSchema: JSONSchema{
				Type:       "object",
				Properties: map[string]SchemaProperty{"name": str("Name of the workspace to delete.")},
				Required:   []string{"name"},
			},
		},
	}
}
