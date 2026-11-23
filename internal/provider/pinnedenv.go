package provider

// ClaudeMCPDescriptionLengthEnv caps, in characters, both an MCP server's
// instructions and each MCP tool description Claude Code shows the model;
// longer text is cut and marked truncated. Unset, the CLI caps instructions
// and up-front tool descriptions at 4096 and descriptions loaded through
// tool search at 16384 (2048 for instructions before 2.1.296); a value
// replaces both caps (2.1.300 bundle). The thread-tools guide is longer than
// 4096, so claude.NewSession defaults the variable to
// ClaudeMCPDescriptionLengthLimit. It is a default, not a pin: a user value
// in the custom environment wins.
const ClaudeMCPDescriptionLengthEnv = "CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH"

// ClaudeMCPDescriptionLengthLimit is the value AO sets for
// ClaudeMCPDescriptionLengthEnv: the CLI's own tool-search cap, so deferred
// tool descriptions keep their limit and instructions and up-front
// descriptions rise to it. The app's MCP server instructions must fit under
// it; threadtools' instructions test enforces that.
const ClaudeMCPDescriptionLengthLimit = 16384
