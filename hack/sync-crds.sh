#!/usr/bin/env bash
# Wrap the controller-gen output into a Helm template.
#
# CRDs live in templates/ rather than crds/ on purpose: Helm never upgrades
# anything in crds/, so a chart upgrade would silently leave an old schema in
# place. Templating them costs an install-time toggle and buys real upgrades.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="${repo_root}/deploy/crds/pgproxy.io_postgresroutes.yaml"
dst="${repo_root}/charts/pg-k8s-proxy/templates/crd-postgresroute.yaml"

if [[ ! -f "${src}" ]]; then
  echo "error: ${src} not found; run 'make manifests' first" >&2
  exit 1
fi

{
  echo '{{- if .Values.crds.install }}'
  echo '{{/*'
  echo 'Generated from src/api/v1alpha1 by "make manifests". Do not edit by hand.'
  echo '*/}}'

  awk '
    # Drop the leading document separator; Helm supplies its own.
    NR == 1 && $0 == "---" { next }

    # Helm needs to own the CRD, and resource-policy keeps user data on uninstall.
    $0 ~ /^    controller-gen\.kubebuilder\.io\/version:/ {
      print
      print "    {{- if .Values.crds.keep }}"
      print "    helm.sh/resource-policy: keep"
      print "    {{- end }}"
      next
    }

    # Chart labels go in just before the name, the last key in metadata.
    $0 == "  name: postgresroutes.pgproxy.io" {
      print "  labels:"
      print "    {{- include \"pg-k8s-proxy.labels\" . | nindent 4 }}"
      print
      next
    }

    { print }
  ' "${src}"

  echo '{{- end }}'
} > "${dst}"

echo "wrote ${dst}"
