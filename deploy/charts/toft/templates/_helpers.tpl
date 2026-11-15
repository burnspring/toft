{{/*
Object names follow the usual Helm conventions and may be overridden.
*/}}
{{- define "toft.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "toft.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "toft.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
THE PLATFORM POD'S IDENTITY LABEL IS A CONSTANT AND MUST NOT BE TEMPLATED.

`app.kubernetes.io/name: toft` is compiled into the binary
(internal/kube/kube.go, managedByValue) and is what the NetworkPolicy the
backend puts on every isolated workload admits ingress from. Derive it from
nameOverride or the release name and the policy stops matching: every isolated
app becomes unreachable through the gateway, with no error in any log — the
pod is running, the policy is applied, and the traffic is simply denied.

This is why the chart offers no knob for it. nameOverride and fullnameOverride
still rename *objects*; they cannot rename this.

Two releases in one cluster therefore share the label. The policy also selects
the platform's own namespace (read from the pod's service-account mount), so a
second release in another namespace is not admitted — but one Toft per cluster
is still the supported shape.
*/}}
{{- define "toft.workloadLabel" -}}toft{{- end }}

{{- define "toft.selectorLabels" -}}
app.kubernetes.io/name: {{ include "toft.workloadLabel" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "toft.labels" -}}
helm.sh/chart: {{ include "toft.chart" . }}
{{ include "toft.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "toft.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "toft.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "toft.secretName" -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else }}
{{- printf "%s-secrets" (include "toft.fullname" .) }}
{{- end }}
{{- end }}

{{- define "toft.configMapName" -}}
{{- printf "%s-config" (include "toft.fullname" .) }}
{{- end }}

{{- define "toft.postgresName" -}}
{{- printf "%s-postgres" (include "toft.fullname" .) }}
{{- end }}

{{- define "toft.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag }}
{{- $repo := required "set image.repository to the registry holding your Toft image (build one with `docker build -t <registry>/toft .`)" .Values.image.repository }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}

{{/*
Single origin is a property of the *addresses*, not of how they were chosen —
exactly as scripts/k3s.sh has always derived it. Everything downstream reads
this one answer, so an install cannot end up with URLs that describe one shape
and ports that implement the other.
*/}}
{{- define "toft.singleOrigin" -}}
{{- $base := trimSuffix "/" .Values.platform.baseUrl }}
{{- $apps := trimSuffix "/" .Values.platform.appsBaseUrl }}
{{- if eq $base $apps }}true{{- else }}false{{- end }}
{{- end }}

{{/*
APPS_PORT == PORT is what actually puts the platform in single-origin mode; the
URLs only describe it. Setting one without the other gives an install whose
links point somewhere nothing is listening.
*/}}
{{- define "toft.appsPort" -}}
{{- if eq (include "toft.singleOrigin" .) "true" }}8080{{- else }}8081{{- end }}
{{- end }}

{{/*
Cookies carry Secure unless told otherwise, and a browser silently drops a
Secure cookie sent over plain http to a host that is not localhost — a sign-in
that appears to work and then does not. So the switch is on when the operator
asks for it, and on anyway when baseUrl is not HTTPS.

**Off is the empty string, never `0`.** The binary reads this name as a boolean
and an earlier release read it as "set to anything at all", which made the `0`
this template used to emit for an HTTPS install mean *on* — every chart install
over HTTPS minting its session cookies without `Secure`. Emitting nothing is the
one spelling both readers agree on, and the key stays present so the image's own
value cannot stand in its place.
*/}}
{{- define "toft.insecureCookies" -}}
{{- if .Values.platform.insecureCookies }}1
{{- else if not (hasPrefix "https://" .Values.platform.baseUrl) }}1
{{- end }}
{{- end }}

{{/*
The hostname out of a browser-facing URL, for ingress rules.
*/}}
{{- define "toft.hostOf" -}}
{{- $u := regexReplaceAll "^https?://" . "" }}
{{- $u = splitList "/" $u | first }}
{{- splitList ":" $u | first }}
{{- end }}

{{- define "toft.platformHost" -}}
{{- default (include "toft.hostOf" .Values.platform.baseUrl) .Values.ingress.platformHost }}
{{- end }}

{{- define "toft.appsHost" -}}
{{- default (include "toft.hostOf" .Values.platform.appsBaseUrl) .Values.ingress.appsHost }}
{{- end }}

{{/*
The annotation that carries ingress.inboundCidrs to an object that enforces it,
or empty when the chart does not know one for ingress.className. Empty is a
refusal in toft.validate, never a silent pass: a list that reaches no object is
the open install the list exists to prevent, with NOTES.txt saying otherwise.
*/}}
{{- define "toft.inboundAnnotation" -}}
{{- $class := .Values.ingress.className | default "" }}
{{- if .Values.ingress.inboundCidrsAnnotation }}{{ .Values.ingress.inboundCidrsAnnotation }}
{{- else if eq $class "alb" }}alb.ingress.kubernetes.io/inbound-cidrs
{{- else if or (eq $class "nginx") (eq $class "webapprouting.kubernetes.azure.com") }}nginx.ingress.kubernetes.io/whitelist-source-range
{{- end }}
{{- end }}

{{/*
True when the rendered Ingress carries the allow-list — the one condition under
which NOTES.txt may say "Reachable only from".
*/}}
{{- define "toft.inboundEnforced" -}}
{{- if and .Values.ingress.enabled .Values.ingress.inboundCidrs (include "toft.inboundAnnotation" .) }}true{{- end }}
{{- end }}

{{/*
True when at least one runnerImages entry names an image.
*/}}
{{- define "toft.anyRunnerImage" -}}
{{- $any := false }}
{{- range $family, $image := .Values.runnerImages }}{{- if $image }}{{- $any = true }}{{- end }}{{- end }}
{{- if $any }}true{{- end }}
{{- end }}

{{/*
True when the install runs isolated workloads: an artifact store to fetch
releases from and at least one runner image to run them in. The binary refuses
to start with KUBE_NAMESPACE set and either missing (config.validateIsolated),
so the isolated block of the ConfigMap is rendered only under this — an install
at the default values is shared mode, not a crash loop. An artifact store with
no runner image stays legal: that is shared mode with a store.
*/}}
{{- define "toft.isolatedEnabled" -}}
{{- if and .Values.artifactStore.enabled (include "toft.anyRunnerImage" .) }}true{{- end }}
{{- end }}

{{/*
Refuse at render time rather than at runtime. Each of these presents as a
working install that is subtly wrong, which is worse than a failed helm install.
*/}}
{{- define "toft.validate" -}}
{{- if not .Values.platform.baseUrl }}
{{- fail "platform.baseUrl is required (how a browser reaches the platform)" }}
{{- end }}
{{- if not .Values.platform.appsBaseUrl }}
{{- fail "platform.appsBaseUrl is required; set it equal to platform.baseUrl to deliberately choose single-origin mode" }}
{{- end }}
{{- if and (not .Values.postgres.enabled) (not .Values.secrets.existingSecret) }}
{{- fail "postgres.enabled=false requires secrets.existingSecret carrying the database URL (under external.databaseUrlKey, default DATABASE_URL)" }}
{{- end }}
{{- if and .Values.artifactStore.enabled (not .Values.artifactStore.bucket) }}
{{- fail "artifactStore.enabled=true requires artifactStore.bucket" }}
{{- end }}
{{- if and (include "toft.anyRunnerImage" .) (not .Values.artifactStore.enabled) }}
{{- fail "runnerImages is set but artifactStore.enabled=false: isolated mode needs an artifact store, because an isolated workload fetches its release rather than sharing the platform's disk. Enable artifactStore, or leave runnerImages empty for a shared-mode install." }}
{{- end }}
{{- if not (include "toft.isolatedEnabled" .) }}
{{- if or .Values.appStorage.efs.enabled .Values.appStorage.s3Files.enabled .Values.appStorage.classes .Values.appStorage.blob.account }}
{{- fail "appStorage (efs, s3Files, classes or blob.account) is set but isolated mode is not: cluster storage is provisioned by the isolated backend, which needs artifactStore.enabled and at least one runnerImages entry. Without them the setting would be silently dropped." }}
{{- end }}
{{- end }}
{{- if and .Values.appStorage.efs.enabled .Values.appStorage.efs.create (not .Values.appStorage.efs.fileSystemId) }}
{{- fail "appStorage.efs.create=true requires appStorage.efs.fileSystemId" }}
{{- end }}
{{- if and .Values.ingress.enabled (eq (include "toft.singleOrigin" .) "false") }}
{{- if eq (include "toft.platformHost" .) (include "toft.appsHost" .) }}
{{- fail "two-origin mode needs two distinct hostnames; platform.baseUrl and platform.appsBaseUrl resolve to the same host" }}
{{- end }}
{{- end }}
{{- if and .Values.ingress.enabled (not .Values.ingress.inboundCidrs) (not .Values.ingress.allowPublicInbound) }}
{{- fail "ingress.enabled=true with no ingress.inboundCidrs: this would publish the install to the internet with sign-in as the only control. Set ingress.inboundCidrs to the addresses that may reach it, or set ingress.allowPublicInbound=true to mean it." }}
{{- end }}
{{- if and .Values.ingress.enabled .Values.ingress.inboundCidrs (not (include "toft.inboundAnnotation" .)) (not .Values.ingress.allowPublicInbound) }}
{{- fail (printf "ingress.inboundCidrs is set but ingress.className %q has no allow-list annotation this chart knows (alb, nginx, webapprouting.kubernetes.azure.com), so nothing would enforce it and the install would be open to the internet. Set ingress.inboundCidrsAnnotation to the annotation your controller enforces, or set ingress.allowPublicInbound=true to mean it." (.Values.ingress.className | default "")) }}
{{- end }}
{{- $inboundKey := include "toft.inboundAnnotation" . }}
{{- if and .Values.ingress.enabled .Values.ingress.inboundCidrs $inboundKey (hasKey (.Values.ingress.annotations | default dict) $inboundKey) }}
{{- fail (printf "ingress.annotations sets %s and ingress.inboundCidrs renders it too; keep the addresses in ingress.inboundCidrs only" $inboundKey) }}
{{- end }}
{{/*
The platform pod's NetworkPolicy admits pods by namespace and everything else
only by address. An ALB in IP mode and a LoadBalancer Service are not pods, so
with no address listed every browser request would be dropped at the pod — an
outage that reads as a load-balancer health problem.
*/}}
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.platformSourceCidrs) }}
{{- if and .Values.ingress.enabled (eq .Values.ingress.className "alb") }}
{{- fail "ingress.className=alb with networkPolicy.enabled and no networkPolicy.platformSourceCidrs: the ALB dials the platform pod from its own subnets, which no namespace rule admits. Set networkPolicy.platformSourceCidrs to the ALB's subnets (never a range holding app pods), or networkPolicy.enabled=false to give up confining apps to the callback port." }}
{{- end }}
{{- if eq .Values.service.type "LoadBalancer" }}
{{- fail "service.type=LoadBalancer with networkPolicy.enabled and no networkPolicy.platformSourceCidrs: traffic arrives from other nodes or from the client, which no namespace rule admits. Set networkPolicy.platformSourceCidrs (the node subnet, or the client ranges with externalTrafficPolicy: Local; never a range holding app pods), or networkPolicy.enabled=false." }}
{{- end }}
{{- end }}
{{/*
An install behind an ingress with trustedProxyCidrs left empty (the default) or
at loopback is the single most confusing misconfiguration available:
X-Forwarded-Proto is ignored, so the platform mints http:// links on an https://
install and every sign-in redirect breaks.
*/}}
{{- if and .Values.ingress.enabled (has (trim .Values.platform.trustedProxyCidrs) (list "" "127.0.0.1/32,::1/128")) }}
{{- fail "ingress.enabled=true with platform.trustedProxyCidrs empty (the default) or loopback: the ingress controller is neither, so X-Forwarded-* would be ignored and the install would mint http:// links. Set platform.trustedProxyCidrs to the ingress controller's own addresses — ideally not the whole pod CIDR, which holds the apps too (values.yaml says how to narrow it, and what it costs where it cannot be)." }}
{{- end }}
{{/*
This value used to be a string, where "" meant "derive it". A string survives
into a boolean's place unnoticed — Go templates read any non-empty one as true,
so an upgrade carrying the old `insecureCookies: "0"` forward would render the
switch ON while the values file says off. Refused here rather than rendered,
because that is the failure this whole flag's history is made of.
*/}}
{{- if not (kindIs "bool" .Values.platform.insecureCookies) }}
{{- fail (printf "platform.insecureCookies must be true or false, got %#v. This value was a string in chart 0.1.1 and is a boolean now: write `insecureCookies: false` (the default, cookies carry Secure) or `insecureCookies: true`." .Values.platform.insecureCookies) }}
{{- end }}
{{- end }}

{{/*
appStorage.classes as KUBE_STORAGE_CLASSES: name[=backing], comma-separated.
*/}}
{{- define "toft.storageClasses" -}}
{{- $out := list -}}
{{- range . -}}
{{- if .backing -}}
{{- $out = append $out (printf "%s=%s" .name .backing) -}}
{{- else -}}
{{- $out = append $out .name -}}
{{- end -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}
