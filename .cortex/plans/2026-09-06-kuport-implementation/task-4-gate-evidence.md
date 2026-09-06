# Task 4 gate evidence — deploy manifests, Helm chart, samples

Recorded 2026-09-06 by impl-task4-deploy (omp, herdr agent impl-task4-deploy),
reported to meta-wave2 by A2A steer on completion and cross-checked against
that session's transcript. The gates were run once, by the worker; the meta
did not re-run them.

## Environment note

Host ~/.kube/config points at a down docker-desktop server, and kubectl 1.37
performs discovery even for --dry-run=client — the same situation task 1
recorded. The worker stood up an envtest control plane (1.33.0 assets via
setup-envtest, harness at .tmp/kubeup/main.go, build-tagged, gitignored, never
committed) and ran the kubectl gates against it; CRDs were registered once via
`kubectl create --save-config -f config/crd/` as setup, which is why they read
"unchanged (dry run)". Harness stopped after the run.

## Worker's recorded gate output

```
$ kubectl apply --dry-run=client -f config/crd/ -f config/samples/
customresourcedefinition.apiextensions.k8s.io/portmapclasses.kuport.dev unchanged (dry run)
customresourcedefinition.apiextensions.k8s.io/portmaps.kuport.dev unchanged (dry run)
portmap.kuport.dev/gameserver created (dry run)
portmap.kuport.dev/gameserver-voice created (dry run)
portmapclass.kuport.dev/public created (dry run)
portmapclass.kuport.dev/intranet created (dry run)

$ kubectl kustomize deploy/            # exit=0, full bundle renders
$ kubectl apply --dry-run=client -k deploy/
namespace/kuport-system created (dry run)
customresourcedefinition... unchanged (dry run) x2
serviceaccount/kuport-agent created (dry run)
clusterrole.rbac.authorization.k8s.io/kuport-agent created (dry run)
clusterrolebinding.rbac.authorization.k8s.io/kuport-agent created (dry run)
daemonset.apps/kuport-agent created (dry run)

$ helm lint chart/
1 chart(s) linted, 0 chart(s) failed

$ helm template kuport chart/                       -> .tmp/render-default.yaml
$ helm template ... --set image.digest=sha256:000..0 -> .tmp/render-digest.yaml
$ helm template ... --set-json 'classes={...}'       -> .tmp/render-class.yaml
$ kubectl apply --dry-run=client -f .tmp/render-default.yaml   all (dry run)
$ kubectl apply --dry-run=client -f .tmp/render-digest.yaml    all (dry run)
$ kubectl apply --dry-run=client -f .tmp/render-class.yaml     all (dry run) + portmapclass public
```

## Negative checks

- grep '@sha256:' .tmp/render-digest.yaml prints the image line with the
  digest (digest wins over tag).
- grep -c 'kind: PortMapClass' .tmp/render-class.yaml -> 1; render-default -> 0
  (empty classes map renders no class).
- diff of the capabilities blocks between `kubectl kustomize deploy/` and the
  chart render is empty: same NET_ADMIN grant in both.

## Versions checked, not assumed

Chart apiVersion v2 stable under helm v4.2.4 (v3 experimental, HIP-0020);
crds/ semantics (installed before templates, never upgraded or deleted)
verified against helm.sh best practices for 4.2.4; system-node-critical
confirmed current in kubernetes.io pod-priority-preemption docs for v1.37;
kustomize apiVersion kustomize.config.k8s.io/v1beta1 (v5.8.1); no Helm library
chart — helpers are local.

## Deviations

1. deploy/kustomization.yaml lists crds as verbatim copies under deploy/crds/
   instead of a ../config/crd reference: kubectl kustomize refuses to load
   anything outside the deploy/ load root, and config/crd/ (read-only source)
   has no kustomization.yaml. Refresh command commented in the file. Task 5's
   `kustomize edit set image` and `kubectl kustomize deploy/` work unchanged;
   the bundle still carries the CRDs.
2. No readiness probe: cmd/kuport-agent does not exist yet (task 6 running),
   and the spec says leave it out rather than invent one. Task 6's spec plans
   /healthz and /readyz on --health-addr default :8081; a follow-up should add
   readinessProbe httpGet /readyz :8081 (+ liveness on /healthz) to
   deploy/daemonset.yaml and the chart once task 6 lands.
3. DaemonSet carries env NODE_NAME (downward API spec.nodeName) and LOG_LEVEL,
   not in the spec's snippet, but task 6's flag table marks NODE_NAME required
   and values.yaml's logLevel must reach the container. Mirrored in deploy/
   and the chart so the defaults render identically.
4. The chart does not template a Namespace (helm install owns the namespace
   via -n/--create-namespace; a templated Namespace conflicts or dead-ends).
   Namespaced objects follow .Release.Namespace; README step 1 installs into
   kuport-system. Everything else is a field-for-field mirror of deploy/; the
   only value drift is by spec: chart appVersion 0.1.0 renders the default tag
   :0.1.0 while deploy/daemonset.yaml pins :v0.1.0, and the release workflow
   re-syncs both.

Committed by meta-wave2 as 79edf8a after verification, scoped to deploy/,
chart/, config/samples/.
