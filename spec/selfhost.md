# Selfhosting the JitTrippin Daemon

The JitTrippin Daemon is just a REST API.
The best way, so far is to run it as a **systemd** service

## 1. Make a directory for the service

```sh
mkdir -p /opt/jittrippin
git clone https://github.com/nxrmqlly/jittrippin.git /opt/jittrippin

cd /opt/jittrippin
```

## 2. Setup PostgreSQL

It's very simple to create a PostgreSQL database with Docker.

```sh
docker run \
    --name jittrippin-postgres \
    -e POSTGRES_USER=admin \
    -e POSTGRES_PASSWORD=jittrippin123 \
    -e POSTGRES_DB=jtdb \
    -p 5432:5432 \
    --restart unless-stopped \
    -d postgres:latest
```

This results in your `JTD_POSTGRES_CONNSTR` to look like:

```
postgres://admin:jittrippin123@localhost:5432/jtdb?sslmode=disable
```

You should use a more secure password rather than "jittrippin123". Set it in the env.

## 3. Setup the Github App

Go to [GitHub Developer Settings](https://github.com/settings/apps/) and create a "New Github App"

> **OAuth App != GitHub App**
>
> JitTrippin uses a GitHub App for both GitHub OAuth and GitHub repository integration.

### Basic Configuration

Give it a name, for example "My JitTrippin" and configure it as the following:

1. **Homepage URL**: `JTD_PUBLIC_URL` from env OR anything else that you want your homepage to be
2. **Redirect URI**: Set it to exactly the same value as `JTD_REDIRECT_URL` from env

> Example: `https://jt.example.com/api/v1/auth/github/callback`

3. **Expire user authorization tokens**: Enabled

4. **Setup URL**: Set to `JTD_PUBLIC_URL` + `/api/v1/integrations/github/install-callback`

> Example: `https://jt.example.com/api/v1/integrations/github/install-callback`

5. **Webhook > Active**: Enable this.
6. **Webhook > Webhook URL**: set it to the `JTD_PUBLIC_URL` + `/api/v1/integrations/github/webhook`

> Example: `https://jt.example.com/api/v1/integrations/github/webhook`

7. **Webhook > Webhook Secret**: Set to the same value you will use for `GITHUB_WEBHOOK_SECRET` (see "Configuring the Environment" section)

### Repository permissions

| Permission        | Access         |
| ----------------- | -------------- |
| Actions           | Read-only      |
| Artifact metadata | Read and write |
| Commit statuses   | Read and write |
| Contents          | Read and write |
| Deployments       | Read and write |
| Issues            | Read and write |
| Pull requests     | Read and write |
| Webhooks          | Read and write |
| Workflows         | Read and write |

### Account permissions:

| Permission      | Access    |
| --------------- | --------- |
| Email addresses | Read-only |

### Events:

Subscribe to these events:

1. Installation target
2. Meta
3. Delete
4. Issues
5. Public
6. Pull Request
7. Pull request review
8. Pull request review comment
9. Pull request review thread
10. Push
11. Release
12. Repository
13. Status

Choose "Only on this account" if you are running JitTrippin privately for yourself.
Choose "Any account" if you intend to let others install your JitTrippin bot/app.

Finally, click **Create GitHub App.**

## Github App ID, ClientID and App Slug

1. Under the "About" section, copy the "App ID". This is the value for `GITHUB_APP_ID` in env.
2. Copy the "Client ID" in the same section. This is the value for `GITHUB_CLIENT_ID` in env.
3. Look at your browser URL bar, it should be something like `https://github.com/settings/apps/my-jittrippin`. `my-jittrippin` is the value for `GITHUB_APP_SLUG`

## 4. GitHub App Private Key and Client Secret

### Client Secret

1. Under "Client secrets" Generate a new Client secret.
2. Save it as you can only see it once.
3. This is the value for `GITHUB_CLIENT_SECRET` in env.

### Private Key

1. Scroll down and find the **Private keys** section and generate a private key.
2. GitHub will download a .pem file. _**KEEP THIS FILE PRIVATE**_
3. Save it at the repository's root (you can name it `github-app.pem` and it will get .gitignore'd automatically)
4. Depending on where you saved this file, or if you have saved it as `github-app.pem` in the repo root, you can set `GITHUB_PRIVATE_KEY_PATH` in env.

## 5. Configuring the Environment

1. cd into the directory where you cloned the repo.
2. `mv example.env .env`

### Generate Secrets

You should use unpredictable random values for both `JTD_SIGNING_SECRET` and `GITHUB_WEBHOOK_SECRET`.

A simple way to generate one is:

```sh
openssl rand -hex 32
```

Run it twice:

```sh
echo "JTD_SIGNING_SECRET=$(openssl rand -hex 32)"
echo "GITHUB_WEBHOOK_SECRET=$(openssl rand -hex 32)"
```

**Do not reuse the same value for both secrets.**

### Populate `.env`

Fill in the rest of .env

By this point you should have every value needed from the previous steps:

- your PostgreSQL connection string (step 2)
- your GitHub App's ID, Client ID, Client Secret, App Slug, and private key path (steps 3-4)

```sh
# --- Database ---
JTD_POSTGRES_CONNSTR=postgres://admin:jittrippin123@localhost:5432/jtdb?sslmode=disable

# --- Core / Networking ---
JTD_SIGNING_SECRET=(generated above)
JTD_PUBLIC_URL=https://jt.example.com
JTD_REDIRECT_URL=https://jt.example.com/api/v1/auth/github/callback
JTD_BIND_ADDR=127.0.0.1:5500

# --- GitHub App ---
GITHUB_CLIENT_ID=(from GitHub App "About" section)
GITHUB_CLIENT_SECRET=(from Github App "Client secrets")
GITHUB_APP_ID=(from GitHub App "About" section)
GITHUB_PRIVATE_KEY_PATH=/opt/jittrippin/github-app.pem
GITHUB_WEBHOOK_SECRET=(generated above)
GITHUB_APP_SLUG=my-jittrippin
```

1. `JTD_BIND_ADDR` is the local address `jtd` listens on; bind it to `127.0.0.1:5500` (IPv4) and let your reverse proxy handle public HTTPS
2. `GITHUB_PRIVATE_KEY_PATH` should correctly point to the `.pem` file you downloaded from GitHub
3. `JTD_PUBLIC_URL` and `JTD_REDIRECT_URL` must exactly match what you entered in the fields in step 3

## 6. Install `jtd` binary

### Option A: Using `go install`:

```sh
go install github.com/nxrmqlly/jittrippin/cmd/jtd@latest
```

Copy the resulting binary somewhere systemd can execute it, for example:

```sh
sudo cp "$(go env GOPATH)/bin/jtd" /usr/local/bin/jtd
```

### Option B: Build from source

If you cloned the repository, you can build `jtd` directly:

```sh
go build -o /usr/local/bin/jtd ./cmd/jtd
```

## 7. Give `jtd` its own user + access to docker

JitTrippin has its own user and uses the host's Docker daemon to run pipeline containers.

```sh
sudo useradd --system --home /opt/jittrippin --shell /usr/sbin/nologin jittrippin
sudo chown -R jittrippin:jittrippin /opt/jittrippin
sudo usermod -aG docker jittrippin
```

Verify:

```sh
sudo -u jittrippin docker ps
```

If this does not work, `jtd` will _not_ be able to execute pipeline jobs.

> The Docker group grants root-equivalent access. Only give this access to a trusted account you jutst created.

## 8. Configure `systemd`

Create the service file:

```sh
sudo touch /etc/systemd/system/jittrippin.service
```

And paste this in:

```ini
[Unit]
Description=JitTrippin Daemon
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=jittrippin
Group=jittrippin

WorkingDirectory=/opt/jittrippin
EnvironmentFile=/opt/jittrippin/.env

ExecStart=/usr/local/bin/jtd

Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Then run this to reload and start jtd:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now jittrippin
sudo systemctl status jittrippin
```

For logs:

```sh
sudo journalctl -u jittrippin -f
```

## 9. Configure your reverse proxy (if you have one)

Configure your reverse proxy / tunnel to point to `jtd` internal addr.

Your public URL must match `.env`:

```env
JTD_PUBLIC_URL=https://jt.example.com
```

## Finally, pat yourself on the back.
