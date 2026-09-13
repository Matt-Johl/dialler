#!/bin/sh
# Prints one line per packet with its TOS byte (tcpdump -v shows "tos 0x..").
# The test greps the container log for the server's address as source.
exec tcpdump -nn -l -v -i eth0 '(udp or tcp) and not port 53' 2>&1
