# Removed CLI Task transports (T192)

The owner authorized removal without a replacement Task engine. The root
Task API, broker Task command, old-client daemon handler, and separate
`claudia/codex` Task API now refuse default/Claude/Codex/Grok/Cursor execution
with `ErrTaskCLIRemoved` (`task_cli_removed` on the wire). Selection and
capability reporting no longer offer these providers for Task mode.

Bedrock and Ollama API Tasks, Judge, seat transports, provider API adapters,
and the existing grant migration are outside the removal and remain intact.
Public historical event parsers remain. T190 does not become implemented by
this deletion; consumers requiring a restricted OMP Task still lack it.

Verification uses actual public entry points, an isolated broker socket with
an older raw client, and surviving API fixtures. The paired Jevons oracle
calls its real jwork handler through Claudia and checks visible refusal plus
a failed durable worker row. No stub replaces Task in that refusal oracle.

Journey exception: this is an intentional pre-dispatch refusal. A real model
must never receive the request, so provider-free call-site tests decide the
contract. No live-provider, credential, shared-fleet, or activation test is
claimed. The full integration gate and broader CLI Session purge remain
separate work. Existing deadline mutations were rehomed to Bedrock/Ollama
fixtures, preserving the guard's red and request's green controls.
