{{- define "joulie.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "joulie.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "joulie.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "joulie.labels" -}}
app.kubernetes.io/name: {{ include "joulie.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "joulie.selectorLabels" -}}
app.kubernetes.io/name: {{ include "joulie.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}


{{/*
joulie.controllerManager renders the controllerManager values section as YAML.
Until the release after next, keys the user still sets under the deprecated
"operator" section are merged on top, so a values file written for the old key
keeps working. Read it with:
  {{- $cm := include "joulie.controllerManager" . | fromYaml }}
*/}}
{{- define "joulie.controllerManager" -}}
{{- $cm := deepCopy (.Values.controllerManager | default dict) -}}
{{- $legacy := .Values.operator | default dict -}}
{{- mergeOverwrite $cm $legacy | toYaml -}}
{{- end -}}

{{/*
joulie.agentPodSpec renders the pod spec of an agent that manages one node.
The DaemonSet calls it with mode "daemonset"; agent-emulated.yaml calls it
with mode "emulated" and node, one entry of agent.emulated.nodes. Sharing it
keeps an emulated agent's pod the pod a real node runs, except for what
emulation changes: a literal NODE_NAME and PATH, the node's hwemu tree in
place of the host's /sys, /proc/cpuinfo and GPU tools, no host network and
no privileges. Call it with:
  {{- include "joulie.agentPodSpec" (dict "root" $ "mode" "daemonset") | nindent 6 }}
*/}}
{{- define "joulie.agentPodSpec" -}}
{{- $v := .root.Values -}}
{{- $a := $v.agent -}}
{{- $emu := eq .mode "emulated" -}}
{{- $node := .node | default dict -}}
{{- $nodeSelector := $a.daemonset.nodeSelector -}}
{{- $root := "" -}}
{{- $tree := "" -}}
{{- if $emu -}}
{{- $nodeSelector = $a.emulated.hostNodeSelector -}}
{{- $name := required "every agent.emulated.nodes entry needs a name" $node.name -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $name) -}}
{{- fail (printf "agent.emulated.nodes: %q is not a Node name" $name) -}}
{{- end -}}
{{- $root = trimSuffix "/" (required "agent.emulated.root is required in agent.mode emulated" $a.emulated.root) -}}
{{- $tree = printf "%s/%s" $root $name -}}
{{- end -}}
{{- $hostNetwork := and (not $emu) $a.hostNetwork -}}
serviceAccountName: joulie-agent
{{- with $a.runtimeClassName }}
runtimeClassName: {{ . }}
{{- end }}
hostNetwork: {{ $hostNetwork }}
dnsPolicy: {{ if $hostNetwork }}ClusterFirstWithHostNet{{ else }}ClusterFirst{{ end }}
{{- with $nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $a.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
containers:
  - name: agent
    image: "{{ $a.image.repository }}:{{ $a.image.tag | default .root.Chart.AppVersion }}"
    imagePullPolicy: {{ $a.image.pullPolicy }}
    ports:
      - name: metrics
        containerPort: {{ $a.service.port }}
        protocol: TCP
    env:
      {{- if $emu }}
      - name: NODE_NAME
        value: {{ $node.name | quote }}
      - name: PATH
        value: {{ $a.emulated.path | quote }}
      {{- else }}
      - name: NODE_NAME
        valueFrom:
          fieldRef:
            fieldPath: spec.nodeName
      {{- end }}
      {{- range $k, $val := $a.env }}
      - name: {{ $k }}
        value: {{ $val | quote }}
      {{- end }}
      {{- if $v.hardwareCatalog.enabled }}
      - name: HARDWARE_CATALOG_PATH
        value: "{{ $v.hardwareCatalog.mountPath }}/{{ $v.hardwareCatalog.key }}"
      {{- end }}
      {{- with $a.extraEnv }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    securityContext:
      privileged: {{ and (not $emu) $a.privileged }}
      runAsUser: 0
      runAsGroup: 0
      allowPrivilegeEscalation: true
      {{- if $emu }}
      {{- /* Root without CAP_DAC_OVERRIDE gets EACCES writing a 0444 file,
      as on kernfs, and still writes the 0644 files root owns in the tree. */}}
      capabilities:
        drop: ["DAC_OVERRIDE"]
      {{- end }}
    volumeMounts:
      - name: host-sys
        mountPath: /host-sys
      {{- if $emu }}
      - name: hwemu-cpuinfo
        mountPath: /proc/cpuinfo
        readOnly: true
      - name: hwemu-state
        mountPath: /emu/state
      - name: hwemu-bin
        mountPath: /emu/bin
        readOnly: true
      - name: hwemu-lib
        mountPath: /emu/lib
        readOnly: true
      {{- if $node.devNvidiactl }}
      - name: hwemu-nvidiactl
        mountPath: /dev/nvidiactl
      {{- end }}
      {{- end }}
      {{- if $v.hardwareCatalog.enabled }}
      - name: hardware-catalog
        mountPath: {{ $v.hardwareCatalog.mountPath }}
        readOnly: true
      {{- end }}
      {{- with $a.extraVolumeMounts }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
      {{- with $node.extraVolumeMounts }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
volumes:
  - name: host-sys
    hostPath:
      path: {{ if $emu }}{{ $tree }}/sys{{ else }}/sys{{ end }}
      type: Directory
  {{- if $emu }}
  - name: hwemu-cpuinfo
    hostPath:
      path: {{ $tree }}/cpuinfo
      type: File
  - name: hwemu-state
    hostPath:
      path: {{ $tree }}/state
      type: Directory
  - name: hwemu-bin
    hostPath:
      path: {{ $tree }}/bin
      type: Directory
  - name: hwemu-lib
    hostPath:
      path: {{ $root }}/lib
      type: Directory
  {{- if $node.devNvidiactl }}
  - name: hwemu-nvidiactl
    hostPath:
      path: {{ $tree }}/dev/nvidiactl
      type: File
  {{- end }}
  {{- end }}
  {{- if $v.hardwareCatalog.enabled }}
  - name: hardware-catalog
    configMap:
      name: {{ $v.hardwareCatalog.configMapName }}
  {{- end }}
  {{- with $a.extraVolumes }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
  {{- with $node.extraVolumes }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- end -}}
