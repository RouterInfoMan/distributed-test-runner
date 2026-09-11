# Worker node for the "process" abstraction: Nomad gives each suite its own
# task directory and process cgroup (exec driver in production; raw_exec here
# because the client itself runs inside a container).
datacenter = "dc1"
data_dir   = "/nomad/data/process"
log_level  = "INFO"
bind_addr  = "0.0.0.0"
name       = "rcp-process-1"

server { enabled = false }

client {
  enabled   = true
  node_pool = "linux-process"
  servers   = ["nomad-server:4647"]

  # Pinned so the fingerprinter does not shell out to /sbin/ip, which a
  # minimal worker image need not carry.
  network_interface = "eth0"

  cpu_total_compute = 1000
  memory_total_mb   = 1024

  meta {
    "dtp.slots"       = "2"
    "dtp.os"          = "linux"
    "dtp.display"     = "xvfb"
    "dtp.rcp_version" = "4.30"
    "dtp.arch"        = "x86_64"
  }
}

plugin "raw_exec" {
  config { enabled = true }
}

advertise {
  http = "nomad-client-process"
  rpc  = "nomad-client-process"
  serf = "nomad-client-process"
}
