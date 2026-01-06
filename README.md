# SIPp Call Emulator Service

## What is this
A simple HTTP service that helps you make SIP calls using SIPp. Each call runs as its own process, and you can control them through a REST API.

## What's New (v2.0)
- **Service Field**: Now you need to specify which number/service to call (we used to incorrectly use the server IP for this)
- **Better Port Handling**: SIP and media traffic now use separate ports to avoid conflicts
- **More Detailed Logs**: We now log all the important details when starting calls
- **Smarter Port Management**: The system automatically allocates three different ports per call to prevent issues

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

### Making a call
```bash
curl -s -X POST http://localhost:8080/api/v1/calls/outgoing \
  -H 'Content-Type: application/json' \
  -d '{
    "remoteHost":"10.0.0.100",
    "remotePort":5060,
    "destination":"my-test-call",
    "service":"1234567890",
    "scenario":"outgoing.xml"
  }'
```

**Note**: You now need to include a `service` field - this is the actual number or service you want to call. We used to mistakenly use the server's IP for this, which didn't work very well.

## What Changed in v2.0

### What you need to provide
- `service`: **This is new and required** - the phone number or service ID you want to call (like "1234567890")
- `remoteHost`: The SIP server's IP address
- `remotePort`: The SIP server's port (usually 5060)
- `scenario`: Which SIPp scenario file to use

### Optional stuff
- `destination`: Just a label for your own tracking

### What the API returns
- `400` if you forget the service field
- `409` if we're already handling too many calls
- `201` when everything works and your call starts

### Response Format
```json
{
  "callId": "uuid-string",
  "state": "CALLING"
}
```

### DTMF not gonna wrk
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

### About the service parameter
The `service` field gets passed to SIPp as the `-s` parameter - this is the actual number or service you want to call (like "1234567890"). Your scenario file can use the `[service]` placeholder to insert this value.

The `destination` field is just for your own reference and logging.

### How ports work
We now use three different ports for each call:
- **SIP Port**: For the actual SIP protocol messages
- **Media Port**: For the audio/RTP stream  
- **Control Port**: So we can send commands to SIPp while it's running

This helps avoid conflicts and makes things more reliable.

## About controlling calls
SIPp lets you send simple commands over UDP - like 'q' to quit nicely or 'Q' to quit immediately.  
We try TCP first, then fall back to UDP if that doesn't work.

If DTMF doesn't work through the control channel, you might need to put it directly in your XML scenario file.

### Docker notes
We use Alpine Linux in the container because it has SIPp readily available in the package manager, unlike some Debian versions where it can be tricky to install.
