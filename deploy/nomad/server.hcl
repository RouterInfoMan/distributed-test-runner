# Nomad server for the PoC: single-node, no ACLs, no TLS.
datacenter = "dc1"
data_dir   = "/nomad/data"
log_level  = "INFO"
bind_addr  = "0.0.0.0"

server {
  enabled          = true
  bootstrap_expect = 1
}

client {
  enabled = false
}

advertise {
  http = "nomad-server"
  rpc  = "nomad-server"
  serf = "nomad-server"
}

ui { enabled = true }
