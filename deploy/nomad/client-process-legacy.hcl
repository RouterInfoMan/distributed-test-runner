# A second process node pinned to an older RCP target platform, so
# "requires": {"rcp_version": "4.26"} in a submission has somewhere to land.
datacenter = "dc1"
data_dir   = "/nomad/data/process-legacy"
log_level  = "INFO"
bind_addr  = "0.0.0.0"
name       = "rcp-process-legacy"

server { enabled = false }

client {
  enabled   = true
  node_pool = "linux-process"
  servers   = ["nomad-server:4647"]

  # Pinned so the fingerprinter does not shell out to /sbin/ip, which a
  # minimal worker image need not carry.
  network_interface = "eth0"

  cpu_total_compute = 500
  memory_total_mb   = 512

  meta {
    "dtp.slots"       = "1"
    "dtp.os"          = "linux"
    "dtp.display"     = "xvfb"
    "dtp.rcp_version" = "4.26"
    "dtp.arch"        = "x86_64"
  }
}

plugin "raw_exec" {
  config { enabled = true }
}

advertise {
  http = "nomad-client-process-legacy"
  rpc  = "nomad-client-process-legacy"
  serf = "nomad-client-process-legacy"
}
