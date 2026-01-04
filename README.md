# SIPp Call Emulator Service (Go) — MVP Outgoing

## What is this
HTTP service that launches SIPp as an external process (1 call = 1 process) for outgoing calls to `remoteHost:remotePort`, supports DTMF, hangup/disconnect, status, health/ready.

## Quick Start (Docker)

### Option 1 — host network (recommended for SIP)
On Linux/VM this is the most predictable option: SIP responses arrive at the host IP without NAT.

```bash
docker compose --profile host up --build
```

API will be available on the host: `http://<VM_IP>:8080/api/v1/health`

> If you want to hard-code LOCAL_IP (VM IP), set the `LOCAL_IP` variable in compose.

### Option 2 — bridge + port forwarding (convenient for local tests)
```bash
docker compose --profile bridge up --build
```

API available on the host: `http://localhost:8080/api/v1/health`

For SIP to external network, bridge/NAT may break return responses (depends on topology). If the call doesn't establish — switch to host mode.

## Example Usage

### Create a call
```bash
curl -s -X POST http://localhost:8080/api/v1/calls/outgoing \
  -H 'Content-Type: application/json' \
  -d '{
    "remoteHost":"192.168.1.100",
    "remotePort":5060,
    "destination":"1010050",
    "scenario":"outgoing.xml"
  }'
```

### DTMF (not gonna wrk, my bad)
```bash
curl -s -X POST http://localhost:8080/api/v1/calls/<CALL_ID>/dtmf \
  -H 'Content-Type: application/json' \
  -d '{"digits":"123","interDigitDelayMs":100}'
```

### Hangup / Disconnect
```bash
curl -s -X POST http://localhost:8080/api/v1/calls/<CALL_ID>/hangup
curl -s -X POST http://localhost:8080/api/v1/calls/<CALL_ID>/disconnect
```

## Scenarios
Scenarios are mounted into the container at `/opt/sipp/scenarios`.

The repository includes a minimal example:
- `scenarios/outgoing.xml`

### Destination (number)
In the code, destination is passed to SIPp via `-s <destination>` (service/user-part). The scenario can use `[service]` or ready-made SIPp templates.

## Note about control channel
SIPp documents remote control via UDP, where `q` = soft quit, `Q` = hard quit.  
In the implementation, the service tries to control the control-port first via TCP, then fallback to UDP.

If your SIPp build doesn't support the `key <digit>` command — DTMF needs to be moved to the XML scenario.


### Why Alpine in runtime
In Debian bookworm (especially slim), the `sipp` package may be missing from standard repositories, causing `apt-get install sipp` to fail. Therefore, the runtime layer is switched to Alpine, where `apk add sipp` works reliably.
