# xdp-mcp

MCP server for IonBridge USB-C charging devices. Exposes device control, monitoring, and diagnostics via the Model Context Protocol over SSE and Streamable HTTP transports.

## Endpoints

All endpoints are prefixed with `/{PSN}/{TOKEN}/` where:

- **PSN** — device serial number (uint64)
- **TOKEN** — authentication token

### Legacy SSE Transport (Claude Desktop, Cursor, etc.)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/{PSN}/{TOKEN}/sse` | Establish SSE event stream, returns session ID |
| POST | `/{PSN}/{TOKEN}/message` | Send JSON-RPC messages (requires session from `/sse`) |

### Streamable HTTP Transport (Codex, etc.)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/{PSN}/{TOKEN}/mcp` | Single endpoint for all JSON-RPC messages (session managed automatically) |

### Other

| Method | Path | Description |
|--------|------|-------------|
| GET | `/{PSN}/SKILL.md` | Device skill manifest (no auth required) |

## Client Configuration

### Claude Desktop / Cursor (SSE)

```json
{
  "mcpServers": {
    "xdplocal": {
      "url": "http://localhost:8080/{PSN}/{TOKEN}/sse"
    }
  }
}
```

### Codex (Streamable HTTP)

```json
{
  "mcpServers": {
    "xdplocal": {
      "url": "http://localhost:8080/{PSN}/{TOKEN}/mcp"
    }
  }
}
```

## Tools (12)

| Tool | Params | Description |
|------|--------|-------------|
| `get_device_info` | — | PSN, model, firmware, WiFi, MACs |
| `get_machine_facts` | — | Static port config, power budget (from DB) |
| `get_port_details` | — | Live port data: V/A/W, protocol, device ID |
| `get_charging_status` | — | Bitmask of which ports are charging |
| `get_temperature_mode` | — | Global device temperature mode |
| `get_port_pd_status` | — | Full USB PD negotiation, cable info, battery |
| `get_port_stats` | `port` | Historical time-series from device memory (auto-paginated) |
| `set_charging_strategy` | `strategy` | 0=Fast, 1=Slow, 7=HighPerf, 8=UltraFast |
| `set_temperature_mode` | `mode` | 0=PowerPriority, 1=TempPriority |
| `set_port_power_allocation` | `power_allocation` | Per-port watts array |
| `turn_on_port` | `ports` | Array of port numbers (1-indexed) |
| `turn_off_port` | `ports` | Array of port numbers (1-indexed) |

## Prompts (6)

| Prompt | Description |
|--------|-------------|
| `charging_brief` | Quick one-liner status of all ports |
| `charging_status` | Full status with PD details, cable info, optional trends |
| `charging_control` | Control actions: strategy, temperature, port on/off, allocation |
| `charging_profile` | Record charging session over time to Lark/Sheets/markdown |
| `charging_comparison` | Compare ports, before/after changes, or time-series |
| `device_diagnostic` | Deep device + cable diagnostics with compatibility analysis |

## Running

```bash
go build -o xdp-mcp .
./xdp-mcp -config xdp-mcp.yaml -logtostderr
```
