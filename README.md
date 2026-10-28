# harmostes-config

The **configuration tier** of the harmostes three-tier split (ADR-0011
amendment, 2026-10-26): the deployment's agentic decisions and workflow
intent live here — as data, delivered by Flux, read by the kernel.

| Tier | Repo | Owns |
|---|---|---|
| Kernel | `tibrezus/harmostes` | orchestration, attempts, gates, UI, the pi seam |
| **Config** | **this repo** | AgentProfiles (models, extensions, harness, compat), Workflow instances, fork definitions*, canonical `plugins/lib`* |
| Infra | `rezusnet/operations/k8s-config` | cluster facts: namespaces, HelmReleases, secrets, routes |

\* lands as their consuming phases ship (#789/#790).

## Layout

```
base/            namespace-agnostic objects (rare — most config is env-scoped)
overlays/dev     applied to the harmostes-dev namespace (live)
overlays/prod    applied to harmostes (lands only after dev verifies)
hack/            conformance harness (envtest against a pinned kernel chart)
```

## Rules

1. **Data, not code.** No TypeScript, no engine scripts — decisions and intent only.
2. **Everything the kernel reads through the API is a CR here**; nothing is
   duplicated from chart values (single-source: the chart sheds its copy when
   a CR takes over).
3. **Fail loud**: CI validates every object against the CRDs of a PINNED
   kernel release before merge (the apiserver is the conformance surface).
4. **Multi-doc YAML**: every file must parse as `yaml.safe_load_all` (the
   partial-line-edit lesson, ops !288).
5. Prod overlay changes ride the same PR → dev-verified → promote flow as
   everything else.
