# go.mcp-wrapper

A modular Go implementation of a Model Context Protocol (MCP) environment designed to orchestrate multiple agent wrappers and tool servers while presenting a single OpenAI-compatible API to clients such as AnythingLLM. The project consists of three cooperating binaries:

- `agent-orchestrator`: the parent agent exposed to AnythingLLM.
- `agent-child`: helper agent wrappers that participate in the orchestrator’s tool chain.
- `mcp-http-tools`: a reference MCP tool server exposing HTTP methods.

All components use mDNS to discover one another, expose rich event logs via Charmbracelet’s `log` package, and keep their configuration aligned through a shared configuration layer.

## High-level flow

1. **Discovery**
   - Every process starts an mDNS browser for `_mcp-http._tcp` and maintains an atomic snapshot of live services.
   - Discovery emits events (`added`, `updated`, `removed`) to subscribers so each binary can log and react in real time.
   - Services announce themselves with TXT metadata that includes their role (`tool`, `agent-wrapper`, `orchestrator`) and optional model identifiers.

2. **Tool exposure & invocation**
   - Tool servers (e.g. `mcp-http-tools`) advertise `role=tool` and implement `/tools/list` plus `/tools/call`.
   - The orchestrator exposes a live OpenAI-style roster at `GET /v1/tools`, aggregating all tools discovered via MCP.
   - When a chat request explicitly asks for a tool (for example `http_get https://example.com` or a JSON payload `{ "tool": "http_get", "arguments": { ... } }`), the orchestrator selects a matching tool server, invokes it, and feeds the result back into the base model before responding to the caller.
   - Agent wrappers perform their own probes (listing tools and optionally hitting `/healthz`) when a new tool server appears so they are ready to collaborate.

3. **Agent hierarchy**
   - `agent-orchestrator` is the only component that exposes the OpenAI-compatible `/v1/chat/completions` endpoint.
   - `agent-child` wrappers are invisible to AnythingLLM; they advertise as `role=agent-wrapper`, consume tools, and can offload work for the orchestrator.
   - The orchestrator can see both `role=tool` and `role=agent-wrapper` services, while each child focuses on tools and parent orchestrators.

4. **Logging**
   - All binaries rely on `internal/logging` for consistent, styled console output.
   - Logs include configuration summaries at startup, discovery events with metadata, periodic heartbeats, and per-tool invocation telemetry.
   - Environment variables like `LOG_LEVEL=debug` and `LOG_NO_COLOR=true` tune verbosity and colouring.

5. **Model selection**
   - Every agent (orchestrator or child) requires a backend model (e.g. Ollama model slug) provided via `--model` or `AGENT_MODEL`.
   - The orchestrator’s API model name (what AnythingLLM sees) defaults to `go-agent-1` but can be overridden.

## Boot sequence (shared behaviour)

1. **Parse configuration**
   - Flags map to environment variables (`--model`, `--api-model`, `--port`, `--ollama-host`, `--advertise`, `--instance`, `--role`).
   - Configuration is logged immediately so container logs show the effective runtime state.

2. **Set up logging**
   - `internal/logging.FromEnv(prefix)` returns a Charmbracelet logger with consistent styling.
   - All runtime events (discovery, tool use, shutdown) report through this logger.

3. **Start discovery**
   - `discovery.New(...).Start(ctx)` launches mDNS browsing plus periodic pruning.
   - Each binary subscribes to discovery events to maintain an in-memory view and emit structured log messages.

4. **Advertise service**
   - If `--advertise` (or `ADVERTISE=true`) is set, the process uses `internal/discovery.Announcer` to publish its presence with appropriate TXT metadata.

5. **Shutdown gracefully**
   - Signal handling (`SIGINT`, `SIGTERM`) triggers HTTP server shutdown (where applicable) and stops discovery/announcements cleanly.

## Binary details

### `cmd/agent-orchestrator`

Purpose: parent agent wrapper presented to AnythingLLM as an OpenAI-compatible model.

Steps:
1. Load orchestrator-specific config (`config.LoadOrchestrator()`), requiring `--model` to pick the Ollama model. Optional overrides: `--api-model`, `--port`, `--ollama-host`, `--advertise`, `--instance`, `--role`.
2. Start mDNS discovery, subscribe to events, and start logging:
   - Logs each discovered agent wrapper (`role=agent-wrapper`) and tool server (`role=tool`).
   - Prints heartbeat counts every 30 seconds (totals for wrappers, tools, others).
3. Instantiate an OpenAI client (using `openai-go`) pointed at your Ollama `/v1` endpoint.
4. Create mediator (`mediator.New`) configured to only consider tool and agent-wrapper services. The mediator:
   - Validates requests.
   - Injects discovery summaries into the message stream for the base model.
   - Parses user requests for explicit tool commands (`http_get`, `http_post`, etc.), invokes the selected MCP tool server, and merges the result back into the model context before generating a response.
   - Provides `ListTools` for the API layer so clients can retrieve the current tool roster via `GET /v1/tools`.
   - Produces OpenAI-formatted responses.
5. Start the API server (`internal/api.Server`) exposing:
   - `GET /v1/models`
   - `POST /v1/chat/completions`
6. If `--advertise` is set, announce itself with TXT metadata (`role=orchestrator`, `model=<backend>`, `api_model=<api>`).
7. Handle process signals to gracefully stop the HTTP server and discovery loops.

### `cmd/agent-child`

Purpose: secondary agent wrapper meant to consume tools and assist the orchestrator without exposing an API.

Steps:
1. Load wrapper config (`config.LoadWrapper()`), again requiring `--model`/`AGENT_MODEL`.
2. Start discovery, subscribe to events:
   - Logs orchestrator discoveries/heartbeats/loss (`role=orchestrator`).
   - Logs tool server events (`role=tool`).
   - Provides heartbeat stats (counts orchestrators & tools).
3. Optionally advertise as `role=agent-wrapper` with model metadata; this allows orchestrator discovery.
4. Each agent wrapper keeps a local MCP client: on tool discovery it lists the available tools, runs a lightweight `http_get` probe against `/healthz`, and logs the outcome so operators can confirm connectivity.
5. The wrapper also exposes its own MCP tool (served on `--port`) that accepts prompts/messages and returns the wrapper model’s response, making the agent itself selectable from the orchestrator’s `/v1/tools` roster. Customize the tool’s description with `--description` to guide planners.
6. Keep running until signalled.

### `cmd/mcp-http-tools`

Purpose: sample MCP tool server implementing HTTP verbs as tools. Useful for testing and demonstration.

Steps:
1. Load tool server config (`config.LoadToolServer()`):
   - Defaults to `--advertise=true` so that orchestrators/agents discover the server.
   - Requires `--port` or `PORT` (default 8080), `--instance`, `--role` (`tool` by default).
2. Register HTTP handlers:
   - `GET /healthz` – health check.
   - `GET /tools/list` – returns tools `http_get`, `http_post`, `http_put`, `http_patch`, `http_delete`.
   - `POST /tools/call` – executes the selected method against a target URL.
3. On each tool invocation, log method, URL, status code, and latency. Response body is truncated to 1 MiB before returning to the caller.
4. Announce as `role=tool` when `--advertise` is enabled.
5. Wrap the HTTP mux with logging middleware so every request also logs method/path/status/duration.

## Quick start (manual)

1. **Run the HTTP tool server**
   ```bash
   go run ./cmd/mcp-http-tools --port 8081 --advertise
   ```
2. **Run an agent child**
   ```bash
   AGENT_MODEL=phi3 -- advertise optional
   go run ./cmd/agent-child --model phi3 --base-url http://ollama:11434/v1 --api-key ollama --advertise
   ```
3. **Run the orchestrator**
   ```bash
   go run ./cmd/agent-orchestrator \
     --model phi3 \
     --api-model go-agent-1 \
     --base-url http://ollama:11434/v1 \
     --api-key ollama \
     --advertise \
     --port 8080
   ```
4. Point AnythingLLM (or any OpenAI API client) at `http://localhost:8080` with model `go-agent-1`.

## Containerisation

Each binary ships with a dedicated Dockerfile under `cmd/<name>/Dockerfile`. A reference `docker-compose.example.yml` is included to demonstrate wiring the services together (orchestrator, a child agent, and the sample HTTP tool server). All services communicate over the internal Docker network only—no host ports are published—so AnythingLLM or other consumers must run in the same compose network. Adjust the compose file to point at your Ollama container (or any other OpenAI-compatible backend) and customise the `--description` flag for each child agent to explain its speciality.

An accompanying `.env.example` documents the environment variables used by the compose file; copy it to `.env` and tailor the values for your deployment (models, ports, per-agent descriptions, etc.).

## Tool Registration Lifecycle

The orchestrator (and child wrappers) rely on the Model Context Protocol to discover tools and register them as OpenAI function definitions before each chat turn. The flow is the same whether a tool server is another agent wrapper or a standalone MCP service:

1. **Discovery event**
   - mDNS reports a new `_mcp-http._tcp` service. The discovery subsystem stores the server metadata (instance name, host/port, TXT records such as `role`, `description`, etc.).
2. **`tools/list` request**
   - The orchestrator’s mediator (or a child agent) calls the MCP server’s `tools/list` endpoint. This returns every tool the server exposes, including description and JSON schema for its parameters.
3. **OpenAI function synthesis**
   - For each tool, the mediator creates a corresponding OpenAI function definition (`shared.FunctionDefinitionParam`), combining the MCP description and metadata about the hosting server (instance, role, TXT attributes).
   - Function names are namespaced with the server instance and tool name (`<instance>__<tool>`). If multiple tools share the same name across servers, the mediator ensures uniqueness.
4. **Chat completion request**
   - Whenever a chat request arrives, the mediator recomputes the current tool roster (to reflect new or removed servers) and passes it as the `Tools` field in `openai.ChatCompletionNewParams`.
   - The orchestrator uses the configured base OpenAI model (e.g. `phi3` served by Ollama) so the model natively understands function calling semantics.
5. **Model chooses a tool**
   - If the model returns `tool_calls`, the mediator executes each call by invoking `tools/call` on the appropriate MCP server with the generated JSON arguments.
   - Responses from the MCP server are wrapped as tool messages (`openai.ToolMessage`) and fed back into the conversation before requesting another completion. The loop continues until the model produces a regular assistant reply.
6. **Child agents as tools**
   - Each `agent-child` process also exposes its own MCP tool (on `--port`) that accepts prompts/messages and uses its OpenAI client to produce a response. Because it advertises `role=agent-wrapper`, the orchestrator adds it to the function roster exactly like any other tool server, allowing dynamic routing through specialized agents.

This dynamic registration ensures the tool list always mirrors the current network topology; no manual configuration is required beyond running the MCP services on the same network segment.

## Configuration reference

| Option / Env             | Applies to             | Description                                                      |
|--------------------------|------------------------|------------------------------------------------------------------|
| `--model`, `AGENT_MODEL` | agent-orchestrator, agent-child | Required backend model slug for your provider (e.g. `qwen2:1.5b`). |
| `--api-model`, `API_MODEL` | agent-orchestrator, agent-child | Name exposed to clients (defaults to `go-agent-1`).               |
| `--port`, `PORT`         | all binaries           | Port to listen on.                                               |
| `--base-url`, `BASE_URL` | agent-orchestrator, agent-child | Base URL of the upstream OpenAI-compatible endpoint (default `http://ollama:11434/v1`). |
| `--api-key`, `API_KEY` / `OPENAI_API_KEY` | agent-orchestrator, agent-child | API key for the upstream endpoint (default `ollama`).           |
| `--advertise`, `ADVERTISE` | all binaries           | Enable mDNS advertisement.                                       |
| `--instance`, `INSTANCE_NAME` | all binaries           | Instance name shown in discovery (defaults to hostname).         |
| `--role`, `ROLE`         | all binaries           | Role reported in TXT records (default per binary).               |
| `LOG_LEVEL`              | all binaries           | `debug`, `info`, `warn`, `error`, `fatal` (default `info`).      |
| `LOG_NO_COLOR`           | all binaries           | `true` to disable ANSI colours.                                  |

## Development and testing

1. Install dependencies: `go mod tidy`
2. Format & build: `gofmt -w . && go build ./...`
3. Optional: run binaries with `LOG_LEVEL=debug` to inspect discovery chatter.

## Extending the system

- **Add more tool servers**: implement the MCP `/tools/list` and `/tools/call` contract, advertise with `role=tool`, and the orchestrator will detect them automatically.
- **Enhance agent-child behaviour**: integrate the MCP client SDK to call tools on behalf of the orchestrator, or embed specialised LLM workflows.
- **Metrics / tracing**: hook the discovery events, tool invocations, and mediator requests into your telemetry stack by replacing or augmenting the logging layer.

---

The repository is intentionally modular: each layer can be developed, tested, or replaced independently while the discovery backbone keeps everything connected. Plug in your own models or tools to tailor the orchestration to your environment.
