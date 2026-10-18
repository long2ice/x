# The extended (may be experimental) features outside the main gost tree.

## Connection defaults

Routers use a 3-second dial timeout per attempt and retry a failed connection
once, allowing the route selector to choose another healthy node. These defaults
apply when no overrides are provided, including configs received by RelayX Agent.

Set `metadata.dialTimeout` to override the timeout and `handler.retries` to
override the retry count. An explicit `handler.retries: 0` disables retries;
omitting it uses the default. Explicit zero is preserved through JSON and YAML
config reloads.


