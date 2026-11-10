# Pearlora backend

Go control backend for the Pearlora winch prototype. It runs the motor in
manual (up/down/stop, hold or latch) or automatic mode (target depth adjusted
by temperature / soil / tilt rules). The ESP32 firmware connects over
WebSocket at `/ws/device`.

- API spec: [`openapi.yaml`](openapi.yaml) (Swagger UI is deployed alongside the backend)
- Terminal client: `go run ./cmd/pearctl` (see `pearctl help`)
- Fake device for testing without hardware: `go run ./cmd/simdevice`

## Run locally

```sh
go run ./cmd/server                 # :8080, settings in ./settings.json
go run ./cmd/simdevice              # optional fake ESP32
go run ./cmd/pearctl                # interactive console
```

## CI/CD (GitHub Actions)

`.github/workflows/ci-cd.yml`:

| Trigger | Jobs |
|---|---|
| Pull request | gofmt + vet + tests, OpenAPI lint |
| Push to `main` | the same, then build + push images to GHCR, then deploy to the VPS |

Images:
- `ghcr.io/<owner>/<repo>`: the backend
- `ghcr.io/<owner>/<repo>-docs`: Swagger UI with the spec

Each image is tagged `latest` and with the commit SHA.

### One-time VPS setup

1. Install Docker with the compose plugin, and let the SSH user run it
   (`sudo usermod -aG docker <user>`, then log in again).
2. Open ports **8080** (API + ESP32) and **8081** (Swagger UI) in the
   firewall.
3. Create a deploy key and add its public key to the VPS user's
   `~/.ssh/authorized_keys`:
   ```sh
   ssh-keygen -t ed25519 -f pearlora_deploy -N ""
   ```

### GitHub settings

Go to **Settings → Secrets and variables → Actions**.

| Kind | Name | Value |
|---|---|---|
| Secret | `VPS_HOST` | VPS IP or hostname |
| Secret | `VPS_USER` | SSH user |
| Secret | `VPS_SSH_KEY` | contents of the private key `pearlora_deploy` |
| Secret | `VPS_PORT` | optional, defaults to 22 |
| Variable | `API_HOST` | host shown by default in Swagger UI "Try it out" (usually the same as `VPS_HOST`) |

The deploy job uses the `production` environment. It is created
automatically on first run, and you can add required reviewers to it.

### After deploying

- API: `http://<vps>:8080/api/status`
- Swagger UI: `http://<vps>:8081`
- Terminal: `go run ./cmd/pearctl -server http://<vps>:8080`
- Firmware: set `BACKEND_HOST` to the VPS IP and keep `BACKEND_PORT` at 8080.

Settings persist in the `pearlora-data` Docker volume across deploys.

**There is no authentication.** Anyone who can reach port 8080 can move the
motor. Restrict the firewall to the IPs you need, or add auth before
leaving it exposed.
