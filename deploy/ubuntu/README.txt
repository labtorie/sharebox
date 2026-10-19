Sharebox server for Ubuntu / Debian
===================================

Before you start
  1. A server with a public IPv4 address (Ubuntu 22.04 / 24.04, Debian 12).
  2. A domain name with an A record pointing at that address,
     e.g.  share.example.org -> 203.0.113.10
  3. Your cloud provider's firewall must allow:
       TCP 80, 443   UDP+TCP 3478   UDP 49160-49999

Install (one command)
  tar xzf sharebox-server-*-linux-*.tar.gz
  cd sharebox-server-*-linux-*
  sudo ./install.sh

  Non-interactive:
  sudo ./install.sh --domain share.example.org --email you@example.org --yes

Then, in the Sharebox desktop app: Settings -> Sharebox server ->
  https://share.example.org

Manage
  systemctl status sharebox-server
  journalctl -u sharebox-server -f
  sudo nano /etc/sharebox/env && sudo systemctl restart sharebox-server
  sudo ./install.sh --upgrade        (from a newer package; keeps the config)
  sudo ./install.sh --uninstall [--purge]

The server stores no files. Relayed transfers are end-to-end encrypted and
pass through the server's bandwidth, so pick a plan with enough traffic.
