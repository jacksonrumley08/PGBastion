# PGBastion Installation Guide

Complete step-by-step installation instructions for Linux and Windows.

---

## Table of Contents

- [Linux Installation](#linux-installation)
  - [RHEL/CentOS/Rocky/Alma](#rhelcentosrockyalma-linux-8-or-9)
  - [Debian/Ubuntu](#debianubuntu)
- [Windows Installation](#windows-installation)
- [Configuration](#configuration)
- [Verifying Installation](#verifying-installation)
- [Troubleshooting](#troubleshooting)

---

## Linux Installation

### RHEL/CentOS/Rocky/Alma Linux 8 or 9

#### Step 1: Install Prerequisites

```bash
# Update system packages
sudo dnf update -y

# Install Go 1.22+ (RHEL 9 / Rocky 9)
sudo dnf install -y golang

# OR for RHEL 8 / Rocky 8 (Go version may be older, install manually):
# Download Go from https://go.dev/dl/
wget https://go.dev/dl/go1.22.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.22.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

# Verify Go installation
go version
# Should output: go version go1.22.0 linux/amd64 (or newer)
```

#### Step 2: Install PostgreSQL 16

```bash
# Install PostgreSQL repository
sudo dnf install -y https://download.postgresql.org/pub/repos/yum/reporpms/EL-$(rpm -E %{rhel})-x86_64/pgdg-redhat-repo-latest.noarch.rpm

# Disable built-in PostgreSQL module (RHEL 8+)
sudo dnf -qy module disable postgresql

# Install PostgreSQL 16
sudo dnf install -y postgresql16-server postgresql16-contrib

# Initialize database
sudo /usr/pgsql-16/bin/postgresql-16-setup initdb

# Start and enable PostgreSQL
sudo systemctl start postgresql-16
sudo systemctl enable postgresql-16

# Verify PostgreSQL is running
sudo systemctl status postgresql-16
```

#### Step 3: Download and Build PGBastion

```bash
# Create directory for source code
mkdir -p ~/src && cd ~/src

# Clone the repository
git clone https://github.com/jacksonrumley08/pgbastion.git
cd pgbastion

# Build the binary
go build -o pgbastion ./cmd/pgbastion/

# Verify build
./pgbastion version

# Install binary to system path
sudo cp pgbastion /usr/local/bin/
sudo chmod +x /usr/local/bin/pgbastion

# Verify installation
pgbastion version
```

#### Step 4: Create PGBastion User and Directories

```bash
# Create system user (no login shell)
sudo useradd -r -s /sbin/nologin -d /var/lib/pgbastion -c "PGBastion HA Daemon" pgbastion

# Create configuration directory
sudo mkdir -p /etc/pgbastion

# Create data directory for Raft state
sudo mkdir -p /var/lib/pgbastion/raft

# Set ownership
sudo chown -R pgbastion:pgbastion /var/lib/pgbastion

# Create log directory (optional, if using file logging)
sudo mkdir -p /var/log/pgbastion
sudo chown pgbastion:pgbastion /var/log/pgbastion
```

#### Step 5: Configure PostgreSQL for Replication

```bash
# Switch to postgres user
sudo -u postgres psql

# Create replication user
CREATE USER replicator WITH REPLICATION ENCRYPTED PASSWORD 'your_secure_password';

# Create pgbastion superuser for health checks
CREATE USER pgbastion WITH SUPERUSER ENCRYPTED PASSWORD 'your_secure_password';

# Exit psql
\q
```

Edit PostgreSQL configuration:

```bash
# Edit postgresql.conf
sudo nano /var/lib/pgsql/16/data/postgresql.conf
```

Add/modify these settings:

```ini
# Replication settings
wal_level = replica
max_wal_senders = 10
max_replication_slots = 10
hot_standby = on

# Listen on all interfaces (adjust for your network)
listen_addresses = '*'
```

Edit pg_hba.conf for replication access:

```bash
sudo nano /var/lib/pgsql/16/data/pg_hba.conf
```

Add these lines (adjust IP ranges for your network):

```
# Replication connections
host    replication     replicator      10.0.0.0/8              scram-sha-256
host    replication     replicator      192.168.0.0/16          scram-sha-256

# PGBastion health check connections
host    all             pgbastion       127.0.0.1/32            scram-sha-256
host    all             pgbastion       10.0.0.0/8              scram-sha-256
```

Restart PostgreSQL:

```bash
sudo systemctl restart postgresql-16
```

#### Step 6: Create PGBastion Configuration

```bash
sudo nano /etc/pgbastion/pgbastion.yaml
```

**For the FIRST node (bootstrap node):**

```yaml
node:
  name: "node1"
  address: "10.0.1.1"  # This node's IP address

cluster:
  peers:
    - "10.0.1.2"  # Other node IPs
    - "10.0.1.3"

raft:
  data_dir: "/var/lib/pgbastion/raft"
  bind_port: 8300
  bootstrap: true  # ONLY true on first node!

memberlist:
  bind_port: 7946

proxy:
  primary_port: 5432
  replica_port: 5433
  drain_timeout: 30s
  connect_timeout: 5s

vip:
  address: "10.0.1.100"  # Virtual IP for primary
  interface: "eth0"       # Network interface name
  renew_interval: 3s

postgresql:
  bin_dir: "/usr/pgsql-16/bin"
  data_dir: "/var/lib/pgsql/16/data"
  port: 5432
  superuser: "postgres"
  replication_user: "replicator"
  connect_dsn: "postgres://pgbastion:your_secure_password@localhost:5432/postgres?sslmode=disable"
  health_interval: 5s
  health_timeout: 5s

failover:
  confirmation_period: 5s
  fencing_enabled: true
  split_brain_protection: true
  quorum_loss_timeout: 30s

api:
  port: 8080
  bind_address: "0.0.0.0"  # Or restrict to specific IP
  auth_token: "your_secret_api_token"  # Generate with: openssl rand -hex 32
  shutdown_timeout: 5s

metrics:
  port: 9090

log:
  level: "info"
```

**For SECOND and THIRD nodes:**

Copy the same configuration but change:

```yaml
node:
  name: "node2"  # or "node3"
  address: "10.0.1.2"  # This node's IP

raft:
  bootstrap: false  # MUST be false on non-bootstrap nodes
```

Set file permissions:

```bash
sudo chown root:pgbastion /etc/pgbastion/pgbastion.yaml
sudo chmod 640 /etc/pgbastion/pgbastion.yaml
```

#### Step 7: Install Systemd Service

```bash
# Copy service file
sudo cp ~/src/pgbastion/systemd/pgbastion.service /etc/systemd/system/

# Edit service file for RHEL
sudo nano /etc/systemd/system/pgbastion.service
```

Modify the `Requires` line:

```ini
Requires=postgresql-16.service
```

Also add ReadWritePaths for PostgreSQL data:

```ini
ReadWritePaths=/var/lib/pgbastion /var/lib/pgsql
```

Reload and enable:

```bash
# Reload systemd
sudo systemctl daemon-reload

# Enable pgbastion to start on boot
sudo systemctl enable pgbastion
```

#### Step 8: Configure Firewall

```bash
# Open required ports
sudo firewall-cmd --permanent --add-port=5432/tcp   # Primary proxy
sudo firewall-cmd --permanent --add-port=5433/tcp   # Replica proxy
sudo firewall-cmd --permanent --add-port=8080/tcp   # REST API
sudo firewall-cmd --permanent --add-port=8300/tcp   # Raft consensus
sudo firewall-cmd --permanent --add-port=9090/tcp   # Prometheus metrics
sudo firewall-cmd --permanent --add-port=7946/tcp   # Memberlist gossip
sudo firewall-cmd --permanent --add-port=7946/udp   # Memberlist gossip

# Reload firewall
sudo firewall-cmd --reload

# Verify
sudo firewall-cmd --list-ports
```

#### Step 9: Configure SELinux (if enforcing)

```bash
# Check SELinux status
getenforce

# Option 1: Create permissive domain for pgbastion (easier)
sudo semanage permissive -a unconfined_service_t

# Option 2: Create custom policy (more secure, recommended for production)
# First, run pgbastion and collect denials:
sudo ausearch -m avc -ts recent | audit2allow -M pgbastion_policy
sudo semodule -i pgbastion_policy.pp

# If you need to allow network binding:
sudo setsebool -P nis_enabled 1
```

#### Step 10: Start PGBastion

```bash
# Start on the BOOTSTRAP node first
sudo systemctl start pgbastion

# Check status
sudo systemctl status pgbastion

# View logs
sudo journalctl -u pgbastion -f

# After bootstrap node is running, start other nodes
# On node2 and node3:
sudo systemctl start pgbastion
```

#### Step 11: Add Non-Bootstrap Nodes to Raft Cluster

From any running node:

```bash
# Add node2 to Raft cluster
curl -X POST http://localhost:8080/raft/add-peer \
  -H "Authorization: Bearer your_secret_api_token" \
  -H "Content-Type: application/json" \
  -d '{"id": "node2", "address": "10.0.1.2:8300"}'

# Add node3 to Raft cluster
curl -X POST http://localhost:8080/raft/add-peer \
  -H "Authorization: Bearer your_secret_api_token" \
  -H "Content-Type: application/json" \
  -d '{"id": "node3", "address": "10.0.1.3:8300"}'
```

---

### Debian/Ubuntu

#### Step 1: Install Prerequisites

```bash
# Update system packages
sudo apt update && sudo apt upgrade -y

# Install Go 1.22+
# Option 1: From official Go downloads (recommended for latest version)
wget https://go.dev/dl/go1.22.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.22.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

# Option 2: From Ubuntu repositories (may be older)
# sudo apt install -y golang-go

# Verify Go installation
go version

# Install build essentials
sudo apt install -y git build-essential
```

#### Step 2: Install PostgreSQL 16

```bash
# Add PostgreSQL APT repository
sudo sh -c 'echo "deb http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list'

# Import repository signing key
wget --quiet -O - https://www.postgresql.org/media/keys/ACCC4CF8.asc | sudo apt-key add -

# Update package list
sudo apt update

# Install PostgreSQL 16
sudo apt install -y postgresql-16 postgresql-contrib-16

# Verify PostgreSQL is running
sudo systemctl status postgresql
```

#### Step 3: Download and Build PGBastion

```bash
# Create directory for source code
mkdir -p ~/src && cd ~/src

# Clone the repository
git clone https://github.com/jacksonrumley08/pgbastion.git
cd pgbastion

# Build the binary
go build -o pgbastion ./cmd/pgbastion/

# Verify build
./pgbastion version

# Install binary to system path
sudo cp pgbastion /usr/local/bin/
sudo chmod +x /usr/local/bin/pgbastion

# Verify installation
pgbastion version
```

#### Step 4: Create PGBastion User and Directories

```bash
# Create system user
sudo useradd -r -s /usr/sbin/nologin -d /var/lib/pgbastion -c "PGBastion HA Daemon" pgbastion

# Create configuration directory
sudo mkdir -p /etc/pgbastion

# Create data directory for Raft state
sudo mkdir -p /var/lib/pgbastion/raft

# Set ownership
sudo chown -R pgbastion:pgbastion /var/lib/pgbastion
```

#### Step 5: Configure PostgreSQL for Replication

```bash
# Switch to postgres user
sudo -u postgres psql

# Create replication user
CREATE USER replicator WITH REPLICATION ENCRYPTED PASSWORD 'your_secure_password';

# Create pgbastion superuser for health checks
CREATE USER pgbastion WITH SUPERUSER ENCRYPTED PASSWORD 'your_secure_password';

# Exit psql
\q
```

Edit PostgreSQL configuration:

```bash
# Edit postgresql.conf
sudo nano /etc/postgresql/16/main/postgresql.conf
```

Add/modify these settings:

```ini
# Replication settings
wal_level = replica
max_wal_senders = 10
max_replication_slots = 10
hot_standby = on

# Listen on all interfaces
listen_addresses = '*'
```

Edit pg_hba.conf:

```bash
sudo nano /etc/postgresql/16/main/pg_hba.conf
```

Add these lines:

```
# Replication connections
host    replication     replicator      10.0.0.0/8              scram-sha-256
host    replication     replicator      192.168.0.0/16          scram-sha-256

# PGBastion health check connections
host    all             pgbastion       127.0.0.1/32            scram-sha-256
host    all             pgbastion       10.0.0.0/8              scram-sha-256
```

Restart PostgreSQL:

```bash
sudo systemctl restart postgresql
```

#### Step 6: Create PGBastion Configuration

```bash
sudo nano /etc/pgbastion/pgbastion.yaml
```

**For the FIRST node (bootstrap node):**

```yaml
node:
  name: "node1"
  address: "10.0.1.1"

cluster:
  peers:
    - "10.0.1.2"
    - "10.0.1.3"

raft:
  data_dir: "/var/lib/pgbastion/raft"
  bind_port: 8300
  bootstrap: true  # ONLY true on first node!

memberlist:
  bind_port: 7946

proxy:
  primary_port: 5432
  replica_port: 5433
  drain_timeout: 30s
  connect_timeout: 5s

vip:
  address: "10.0.1.100"
  interface: "eth0"
  renew_interval: 3s

postgresql:
  bin_dir: "/usr/lib/postgresql/16/bin"
  data_dir: "/var/lib/postgresql/16/main"
  port: 5432
  superuser: "postgres"
  replication_user: "replicator"
  connect_dsn: "postgres://pgbastion:your_secure_password@localhost:5432/postgres?sslmode=disable"
  health_interval: 5s
  health_timeout: 5s

failover:
  confirmation_period: 5s
  fencing_enabled: true
  split_brain_protection: true
  quorum_loss_timeout: 30s

api:
  port: 8080
  bind_address: "0.0.0.0"
  auth_token: "your_secret_api_token"
  shutdown_timeout: 5s

metrics:
  port: 9090

log:
  level: "info"
```

Set file permissions:

```bash
sudo chown root:pgbastion /etc/pgbastion/pgbastion.yaml
sudo chmod 640 /etc/pgbastion/pgbastion.yaml
```

#### Step 7: Install Systemd Service

```bash
# Copy service file
sudo cp ~/src/pgbastion/systemd/pgbastion.service /etc/systemd/system/

# The default service file should work for Debian/Ubuntu
# Just verify the postgresql.service dependency is correct

# Reload systemd
sudo systemctl daemon-reload

# Enable pgbastion
sudo systemctl enable pgbastion
```

#### Step 8: Configure Firewall (UFW)

```bash
# Enable UFW if not already enabled
sudo ufw enable

# Allow required ports
sudo ufw allow 5432/tcp   # Primary proxy
sudo ufw allow 5433/tcp   # Replica proxy
sudo ufw allow 8080/tcp   # REST API
sudo ufw allow 8300/tcp   # Raft consensus
sudo ufw allow 9090/tcp   # Prometheus metrics
sudo ufw allow 7946/tcp   # Memberlist gossip
sudo ufw allow 7946/udp   # Memberlist gossip

# Verify
sudo ufw status
```

#### Step 9: Start PGBastion

```bash
# Start on the BOOTSTRAP node first
sudo systemctl start pgbastion

# Check status
sudo systemctl status pgbastion

# View logs
sudo journalctl -u pgbastion -f
```

---

## Windows Installation

> **Note:** VIP (Virtual IP) management is NOT supported on Windows. PGBastion will run without VIP functionality. Use a load balancer or DNS-based failover instead.

#### Step 1: Install Prerequisites

**Install Go:**

1. Download Go from https://go.dev/dl/ (choose the Windows AMD64 installer)
2. Run the installer (e.g., `go1.22.0.windows-amd64.msi`)
3. Follow the installation wizard (default settings are fine)
4. Open a NEW Command Prompt or PowerShell and verify:

```powershell
go version
# Should output: go version go1.22.0 windows/amd64
```

**Install Git:**

1. Download Git from https://git-scm.com/download/win
2. Run the installer with default settings
3. Verify in a new terminal:

```powershell
git --version
```

**Install PostgreSQL 16:**

1. Download PostgreSQL from https://www.enterprisedb.com/downloads/postgres-postgresql-downloads
2. Run the installer
3. During installation:
   - Set a password for the postgres superuser
   - Keep the default port (5432)
   - Select your locale
4. Complete the installation

#### Step 2: Download and Build PGBastion

Open PowerShell as Administrator:

```powershell
# Create directory for source code
mkdir C:\src
cd C:\src

# Clone the repository
git clone https://github.com/jacksonrumley08/pgbastion.git
cd pgbastion

# Build the binary
go build -o pgbastion.exe ./cmd/pgbastion/

# Verify build
.\pgbastion.exe version

# Create installation directory
mkdir C:\PGBastion
mkdir C:\PGBastion\data
mkdir C:\PGBastion\config

# Copy binary
copy pgbastion.exe C:\PGBastion\
```

#### Step 3: Configure PostgreSQL for Replication

Open pgAdmin or psql and run:

```sql
-- Create replication user
CREATE USER replicator WITH REPLICATION ENCRYPTED PASSWORD 'your_secure_password';

-- Create pgbastion user for health checks
CREATE USER pgbastion WITH SUPERUSER ENCRYPTED PASSWORD 'your_secure_password';
```

Edit `postgresql.conf` (typically at `C:\Program Files\PostgreSQL\16\data\postgresql.conf`):

```ini
# Replication settings
wal_level = replica
max_wal_senders = 10
max_replication_slots = 10
hot_standby = on

# Listen on all interfaces
listen_addresses = '*'
```

Edit `pg_hba.conf` (same directory):

```
# Replication connections (adjust IP range for your network)
host    replication     replicator      10.0.0.0/8              scram-sha-256
host    replication     replicator      192.168.0.0/16          scram-sha-256

# PGBastion connections
host    all             pgbastion       127.0.0.1/32            scram-sha-256
host    all             pgbastion       10.0.0.0/8              scram-sha-256
```

Restart PostgreSQL service:

```powershell
Restart-Service postgresql-x64-16
```

#### Step 4: Create PGBastion Configuration

Create `C:\PGBastion\config\pgbastion.yaml`:

```yaml
node:
  name: "node1"
  address: "10.0.1.1"  # This server's IP address

cluster:
  peers:
    - "10.0.1.2"
    - "10.0.1.3"

raft:
  data_dir: "C:\\PGBastion\\data\\raft"
  bind_port: 8300
  bootstrap: true  # ONLY true on first node!

memberlist:
  bind_port: 7946

proxy:
  primary_port: 6432    # Use different port than PostgreSQL
  replica_port: 6433
  drain_timeout: 30s
  connect_timeout: 5s

# VIP is NOT supported on Windows - leave empty or omit
vip:
  address: ""
  interface: ""

postgresql:
  bin_dir: "C:\\Program Files\\PostgreSQL\\16\\bin"
  data_dir: "C:\\Program Files\\PostgreSQL\\16\\data"
  port: 5432
  superuser: "postgres"
  replication_user: "replicator"
  connect_dsn: "postgres://pgbastion:your_secure_password@localhost:5432/postgres?sslmode=disable"
  health_interval: 5s
  health_timeout: 5s

failover:
  confirmation_period: 5s
  fencing_enabled: true
  split_brain_protection: true
  quorum_loss_timeout: 30s

api:
  port: 8080
  bind_address: "0.0.0.0"
  auth_token: "your_secret_api_token"
  shutdown_timeout: 5s

metrics:
  port: 9090

log:
  level: "info"
```

#### Step 5: Configure Windows Firewall

Open PowerShell as Administrator:

```powershell
# Allow PGBastion ports
New-NetFirewallRule -DisplayName "PGBastion Primary Proxy" -Direction Inbound -Port 6432 -Protocol TCP -Action Allow
New-NetFirewallRule -DisplayName "PGBastion Replica Proxy" -Direction Inbound -Port 6433 -Protocol TCP -Action Allow
New-NetFirewallRule -DisplayName "PGBastion API" -Direction Inbound -Port 8080 -Protocol TCP -Action Allow
New-NetFirewallRule -DisplayName "PGBastion Raft" -Direction Inbound -Port 8300 -Protocol TCP -Action Allow
New-NetFirewallRule -DisplayName "PGBastion Metrics" -Direction Inbound -Port 9090 -Protocol TCP -Action Allow
New-NetFirewallRule -DisplayName "PGBastion Memberlist TCP" -Direction Inbound -Port 7946 -Protocol TCP -Action Allow
New-NetFirewallRule -DisplayName "PGBastion Memberlist UDP" -Direction Inbound -Port 7946 -Protocol UDP -Action Allow
```

#### Step 6: Install as Windows Service (using NSSM)

Download NSSM (Non-Sucking Service Manager):

1. Download from https://nssm.cc/download
2. Extract to `C:\PGBastion\nssm\`

Install PGBastion as a service:

```powershell
# Install service
C:\PGBastion\nssm\win64\nssm.exe install PGBastion "C:\PGBastion\pgbastion.exe" "start --config C:\PGBastion\config\pgbastion.yaml"

# Configure service
C:\PGBastion\nssm\win64\nssm.exe set PGBastion AppDirectory "C:\PGBastion"
C:\PGBastion\nssm\win64\nssm.exe set PGBastion DisplayName "PGBastion HA Daemon"
C:\PGBastion\nssm\win64\nssm.exe set PGBastion Description "PostgreSQL High-Availability Daemon"
C:\PGBastion\nssm\win64\nssm.exe set PGBastion Start SERVICE_AUTO_START
C:\PGBastion\nssm\win64\nssm.exe set PGBastion AppStdout "C:\PGBastion\logs\stdout.log"
C:\PGBastion\nssm\win64\nssm.exe set PGBastion AppStderr "C:\PGBastion\logs\stderr.log"

# Create logs directory
mkdir C:\PGBastion\logs

# Set service to depend on PostgreSQL
sc.exe config PGBastion depend= postgresql-x64-16

# Start the service (on bootstrap node first)
Start-Service PGBastion

# Check status
Get-Service PGBastion
```

#### Step 7: Running Without a Service (for testing)

For testing, you can run PGBastion directly:

```powershell
cd C:\PGBastion
.\pgbastion.exe start --config C:\PGBastion\config\pgbastion.yaml
```

---

## Configuration

### Generating a Secure API Token

```bash
# Linux/macOS
openssl rand -hex 32

# Windows PowerShell
[System.Convert]::ToBase64String([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
```

### Network Interface Names

Find your network interface name:

```bash
# Linux
ip link show
# or
ip addr

# Windows (for reference only - VIP not supported)
Get-NetAdapter
```

### TLS Configuration (Optional)

Generate certificates:

```bash
# Generate CA
openssl genrsa -out ca.key 4096
openssl req -new -x509 -days 3650 -key ca.key -out ca.crt -subj "/CN=PGBastion CA"

# Generate server certificate
openssl genrsa -out server.key 2048
openssl req -new -key server.key -out server.csr -subj "/CN=pgbastion"
openssl x509 -req -days 365 -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out server.crt
```

Add to configuration:

```yaml
raft:
  tls_enabled: true
  tls_cert: "/etc/pgbastion/server.crt"
  tls_key: "/etc/pgbastion/server.key"
  tls_ca: "/etc/pgbastion/ca.crt"  # Enables mTLS

api:
  tls_cert: "/etc/pgbastion/server.crt"
  tls_key: "/etc/pgbastion/server.key"
```

---

## Verifying Installation

### Check Cluster Status

```bash
# Using CLI
pgbastion status --config /etc/pgbastion/pgbastion.yaml

# Using API
curl http://localhost:8080/health
curl http://localhost:8080/cluster
curl http://localhost:8080/nodes
```

### Check Raft Cluster

```bash
# View Raft peers
curl http://localhost:8080/raft/peers

# View Raft leader
curl http://localhost:8080/raft/leader
```

### Check Metrics

```bash
curl http://localhost:9090/metrics
```

### Web UI

Open in browser: `http://<server-ip>:8080/ui/`

---

## Troubleshooting

### PGBastion Won't Start

```bash
# Check logs
sudo journalctl -u pgbastion -n 100 --no-pager

# Common issues:
# 1. Port already in use
sudo ss -tlnp | grep -E '5432|5433|8080|8300|9090'

# 2. Permission denied on Raft data directory
sudo ls -la /var/lib/pgbastion/

# 3. Configuration syntax error
pgbastion start --config /etc/pgbastion/pgbastion.yaml
```

### Raft Cluster Not Forming

```bash
# Check if ports are open between nodes
nc -zv 10.0.1.2 8300

# Check firewall
sudo firewall-cmd --list-ports  # RHEL
sudo ufw status                  # Ubuntu

# Verify bootstrap node is running first
curl http://10.0.1.1:8080/health
```

### VIP Not Assigned (Linux)

```bash
# Check if VIP is assigned
ip addr show eth0

# Check capabilities
getcap /usr/local/bin/pgbastion

# PGBastion needs CAP_NET_ADMIN and CAP_NET_RAW
# These are set via systemd AmbientCapabilities
```

### PostgreSQL Connection Failures

```bash
# Test connection manually
psql "postgres://pgbastion:password@localhost:5432/postgres"

# Check pg_hba.conf allows connections
sudo cat /var/lib/pgsql/16/data/pg_hba.conf | grep pgbastion
```

### SELinux Denials (RHEL)

```bash
# Check for denials
sudo ausearch -m avc -ts recent

# Generate and apply policy
sudo ausearch -m avc -ts recent | audit2allow -M pgbastion_local
sudo semodule -i pgbastion_local.pp
```
