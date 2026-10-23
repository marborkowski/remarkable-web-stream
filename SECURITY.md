# Security policy

This is an experimental hobby project maintained in spare time. It has not been audited. Before deploying it, read the [Security considerations](README.md#security-considerations) in the README, which list the known limitations and risks.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Report them privately through GitHub's [private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability) on this repository (*Security → Report a vulnerability*). Please include:

- what is affected (proxy, tunnel scripts, a specific patch, …) and the version or commit;
- steps to reproduce, or a proof of concept;
- the impact as you understand it.

Reports are handled on a best-effort basis. There is no guaranteed response time and no bug bounty.

## Scope

In scope:
- the `rmshare` proxy (`proxy/`): authentication (password and invite links), roles, session handling and the state file, the remote pen WebSocket, and anything reachable through the proxy;
- the deployment and device scripts (`deploy.sh`, `device/`);
- the goMarkableStream patches in `gms/`, as far as they introduce the problem.

Out of scope: vulnerabilities in upstream goMarkableStream, cloudflared, Tailscale or the reMarkable OS itself. Please report those to their respective projects.
