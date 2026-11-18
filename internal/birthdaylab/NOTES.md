# Notes

Observations from early runs of `burrow birthday`. They are small samples and
only show feasibility; rerun the experiment on your own networks before drawing
conclusions.

## Same-NAT baseline

Server and client on one machine behind one NAT, 256 x 256 candidates over
ports 1024-65535, 5 trials. 2 of 5 trials succeeded and both sides agreed on
the successful sessions and validated the token on the reversed 4-tuple.

This only validates the mechanics (signaling order, a fixed server source port,
256 random SYNs, many client mappings, simultaneous-open validation). Hairpin
traffic through one NAT says nothing about the success rate between two
different NATs.

## Fixed endpoint vs. a carrier-grade NAT

Server on a cloud host with a one-to-one public IP (`-mode fixed`); client on
a laptop tethered to a phone, so its traffic crossed the phone's NAT and then a
mobile carrier's address-and-port-dependent CGNAT. 256 x 256 candidates over
ports 1024-65535, 10 trials of 3 s each.

- 5 of 10 trials succeeded (theory: 63.8% per trial; 95% Wilson interval of
  the observation: 23.7%-76.3%).
- Empirical success rate over three attempts: 87.5%.
- The client public ports that matched were scattered across the whole range,
  so scanning a contiguous neighborhood of a predicted port would have missed
  them.

Conclusion: when one side has a stable public port, a full-range random
birthday collision can get through even two layers of NAT including an APDM
carrier NAT. Around 100 trials are needed for a rate precise enough to base
design decisions on.

## Open questions

- A stable side behind a real NAT (`-mode nat`) rather than a public host:
  does its mapping stay stable across destination IPs, and does it accept the
  simultaneous-open SYNs?
- How much of the gap to the theoretical rate comes from carrier port pools,
  SYN rate limits, filtering or RSTs? Packet captures on both sides (SYN,
  SYN-ACK and RST counts) would show this.
