# Diagnostic environment names

This package defines the environment-variable names shared by entry points and
the `Passthrough` list forwarded across WSL boundaries. It deliberately does
not parse values or apply diagnostic policy; the package that implements each
feature owns those rules.

Add a variable here only when multiple launch boundaries need the same spelling.
Keep `Passthrough` complete and return a fresh slice so callers may modify it.
