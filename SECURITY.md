# Security Policy

> Goolify is an independent project maintained by Worachote Pansanit. It is based on
> [Coolify](https://github.com/coollabsio/coolify) by Andras Bacsai and the CoolLabs community.
> **Please report Goolify vulnerabilities to Goolify, not to the Coolify team.**
> If a vulnerability also affects upstream Coolify, see [Upstream issues](#upstream-issues) below.

## Supported Versions

| Version | Supported | Support Status |
| ------- | --------- | -------------- |
| `main` / latest release | :white_check_mark: | Active development and security fixes |
| Older releases | :x: | Please upgrade to the latest release |

## Reporting a Vulnerability

1. **Do not** open a public issue, discussion, or pull request for the vulnerability.
2. Report it privately through GitHub: open the **Security** tab of the Goolify repository and choose
   **Report a vulnerability** (GitHub Private Vulnerability Reporting).
3. Include:
   - A description of the vulnerability and the affected component (`backend/`, `executor/`, `apps/web/`, Docker packaging)
   - Steps to reproduce
   - The privilege the attack needs and the potential impact

You will receive an acknowledgement as soon as possible. Please give us reasonable time to release a fix before any public disclosure.

## Upstream issues

Goolify reuses Coolify's data model, service templates, and threat model. If you believe the same
problem also exists in upstream Coolify, please report it **separately and responsibly** to the
Coolify maintainers as described in the
[upstream security policy](https://github.com/coollabsio/coolify/blob/main/SECURITY.md).
Do not send Goolify-only reports to the Coolify team; they do not maintain this project.

## Scope and Threat Model

*The threat model below is adopted from the upstream Coolify security policy (Apache License 2.0), because Goolify keeps the same roles and authorization rules.*

Please read this section before you send a report. The severity of a report depends on the security boundary that it crosses.

### Roles and what they can already do

- **Team member**: read-only access to the resources of the team. A member must not see secrets or change anything.
- **Member with access grants**: an admin can give a member more levels (such as write or deploy) on the whole team, on projects, on environments, or on resources. A member with write access on a resource can deploy any Docker Compose file for it, including privileged containers and bind mounts of the host file system. That member therefore has root access to the servers where these resources run. In the root team, this includes the Goolify host when a resource runs on the localhost server. Give write access in the root team only to people that you trust as instance admins.
- **Team admin / owner**: full control of the team's servers. An admin can open a terminal on these servers, deploy any Docker Compose file (including privileged containers and bind mounts of the host file system), run scheduled commands, and read all team secrets. An admin already has root access to every server of the team.
- **API tokens**: a token has the permissions of its abilities. A token with `write` or `root` ability is equal to a team admin. A token limited to projects, environments, or resources is limited like a member with access grants: with `write`, it has root access to the servers where the resources inside its limit run.
- **Instance admin** (admin or owner of the root team): already controls the Goolify instance and the server it runs on.
- **Repository and Compose content**: Goolify runs what the configured repository, Dockerfile, or Compose file tells it to run. People who can change this content are trusted for the deployments that use it.

### In scope

- A user who is not logged in can read data, change data, or run code.
- A team member gets write access, runs commands, or reads secrets of their own team.
- A member with access grants reads or changes resources, servers, or credentials outside the grants (for example, places a resource on a server that runs no resource of the grant).
- A user of one team reads or changes resources of another team.
- A team user runs code inside the Goolify container or on the Goolify host, without being an instance admin. Exception: a member with write access on a resource that runs on the localhost server (see the roles above).
- An API token does more than its abilities allow.
- A public webhook or callback can be used without a valid secret or signature.
- The Rust executor runs a command outside its host boundary (`HOST_BOUNDARY_DENIED` bypass).

### Low severity (still welcome)

Please still report these. They are often good findings, and we fix them. Because the attacker already has the same power through normal features, we rate them as low severity:

- Command injection, path traversal, file read or write, or container escape on a server that the attacker's own team manages, when the attack needs the admin or owner role (or a `write` token).
- Problems that need a malicious repository, Dockerfile, or Compose file that a team admin chose to deploy.
- Server-side requests that a team admin can already make from the team's own servers.

If you find a way to reach one of these with less privilege (for example, as a member, from another team, or without login), it is in scope with a higher severity.

### Out of scope

- Attacks that need the instance admin role.
- Reports based only on automated scanner output, without a working attack path.
- Use of the **default development credentials** in `compose.yml` (`APP_KEY`, `ROOT_USER_PASSWORD`). They are documented as local-only and must be changed in production.
- Intended behavior that is listed in [Intended behavior](#intended-behavior-not-a-vulnerability).

### Intended behavior (not a vulnerability)

These are design decisions. Do not report them. We close reports about them without a fix.

- **MCP server and API access are separate switches.** On a self-hosted instance, the **MCP server** setting (instance and team) controls `/mcp`. The REST **API access** setting does not control MCP. MCP can read and change resources while REST API access is disabled. To remove MCP access, disable **MCP server** or revoke the token. **Allowed IPs**, token abilities, and the token team still apply to MCP.

### Severity

Rate the report with the real privilege that the attack needs. If an attack needs the admin role, use `PR:H`. Use `S:C` only when the impact reaches another team, the Goolify instance, or a server that the attacker does not manage.
