# Security Policy

## Supported Versions

Only the latest release, or the default branch if there are no releases,
receives security fixes.

## Reporting a Vulnerability

Please do not report security vulnerabilities through public issues, pull
requests, or discussions.

Report them privately through GitHub's private vulnerability reporting:

1. Go to the repository's Security tab.
2. Click **Report a vulnerability**.

If that option is unavailable, please contact me using the email address on
my GitHub profile.

Please include:

- A description of the issue and its potential impact
- Steps to reproduce, or a proof of concept
- Affected version, commit, or configuration

## What to Expect

This is a personal project maintained on a best-effort basis. I aim to
acknowledge reports within 7 days and will keep you updated on progress.
Once a fix is released, you will be credited in the advisory unless you
prefer to remain anonymous.

## System and Scope

This policy covers the entire metrics_proxy repository, including the Go
proxy, configuration handling, metrics parsers, terminal and CSV output,
dependencies, and build and release workflows.

The proxy forwards HTTP requests to an operator-configured LLM server,
primarily LM Studio or Ollama, and records inference metrics. It is intended
for use only on a local network, not for public internet deployment.
It listens on all interfaces and provides no client authentication.
Network access restrictions are the operator's responsibility.

## Threat Model and Trust Boundaries

The operator controls startup arguments, configuration, the upstream
destination, and the CSV output path. These are trusted administrative
inputs. Remote requests must not gain equivalent control.

Treat client requests, including paths and headers, as untrusted. Consider
both clients that can reach the listening port and malicious websites
attempting to reach it through a local user's browser.

The upstream destination is selected by the operator, but response headers,
payloads, model names, and other returned text are untrusted parser and
output inputs.

Assets include the upstream's models and account operations, proxy and
upstream availability, request and response confidentiality, and the
integrity of terminal output and exported CSV data.

## Security Invariants

- Host validation must occur before forwarding. With restrictive
  `allowed_hosts`, disallowed names must not reach the upstream. This is a
  DNS rebinding control, not client authentication.
- Paths containing decoded `..` segments, including backslash-separated
  segments, must be rejected before forwarding in every configuration.
- With `forward_management: false`, protected management endpoints and
  their descendants must not reach the upstream, regardless of metering
  settings. Path interpretation differences must not bypass this control.
- With management forwarding disabled, protocol upgrades must not create
  a tunnel around request checks. An h2c-only offer may be removed and the
  request forwarded as ordinary HTTP.
- Client-controlled request targets must not override the configured
  upstream destination.
- Metrics inspection must remain bounded: 16 MiB capture limits, eight
  wrapper hops, and 32 MiB cumulative JSON decoding per document. Malformed
  or oversized input must not cause a panic or bypass these bounds.
- Untrusted text must not inject terminal controls or forged log lines.
  CSV text cells must retain formula-injection protection.
- Metrics and debug logging must not expose request or response bodies.

## Reportable Findings and Severity Context

Report reachable violations of these boundaries, including management or
upgrade bypasses, DNS rebinding bypasses, unintended forwarding destinations,
parser resource amplification, output injection, and unintended disclosure.
Dependency and workflow vulnerabilities are relevant when they have a
credible impact on this project.

Assess severity using the required configuration, attacker access, user
interaction, and demonstrated impact. Local-network deployment does not
make reachable vulnerabilities harmless. Tests indicate intended behavior;
they are not proof that a control cannot be bypassed.

## Accepted Behavior and Known Limitations

Unauthenticated inference, including inference-triggered model loading, is
intended behavior for clients that can reach the proxy. Explicitly enabling
`forward_management` permits management requests and protocol upgrades.
These behaviors alone are not findings; bypassing configured restrictions
remains reportable.

Host filtering does not stop direct clients using allowed hosts. Setting
`allowed_hosts: ["*"]` disables that filter. The management endpoint list is
not a guarantee that every operation of every upstream is restricted.

Capture limits do not bound total connections or stream duration. Long
generations are supported without request-body or response-write timeouts.
Traffic uses plain HTTP. These limitations are context for assessment, not
blanket exclusions for denial of service or confidentiality findings.
