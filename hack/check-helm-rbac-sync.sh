#!/usr/bin/env bash
# hack/check-helm-rbac-sync.sh
#
# Verifies that every RBAC permission from the kubebuilder-generated role
# (config/rbac/role.yaml) is present in the Helm chart ClusterRole
# (charts/openclaw-operator/templates/rbac.yaml).
#
# The generated role is the source of truth (derived from +kubebuilder:rbac
# markers). The Helm chart may be a superset but must not be missing any
# permissions.

set -euo pipefail

GENERATED="config/rbac/role.yaml"
HELM="charts/openclaw-operator/templates/rbac.yaml"
HELPERS="charts/openclaw-operator/templates/_helpers.tpl"

if [ ! -f "$GENERATED" ]; then
  echo "::error::Generated RBAC not found at $GENERATED -- run 'make manifests' first"
  exit 1
fi
if [ ! -f "$HELM" ]; then
  echo "::error::Helm chart RBAC not found at $HELM"
  exit 1
fi
if [ ! -f "$HELPERS" ]; then
  echo "::error::Helm chart helpers not found at $HELPERS"
  exit 1
fi

# Parse kubebuilder-generated role.yaml (multi-line YAML) into
# sorted "apiGroup|resource|verb" triples, one per line.
parse_generated() {
  awk '
    /^- apiGroups:/ {
      # Flush previous rule
      for (r = 0; r < nres; r++)
        for (v = 0; v < nvrb; v++)
          print grp "|" res[r] "|" vrb[v]
      delete res; delete vrb; grp = ""; nres = 0; nvrb = 0
      section = "groups"
      next
    }
    /^  resources:/ { section = "resources"; next }
    /^  verbs:/     { section = "verbs";     next }

    section == "groups" && /^  - / {
      s = $0; sub(/^  - /, "", s); gsub(/"/, "", s); grp = s
    }
    section == "resources" && /^  - / {
      s = $0; sub(/^  - /, "", s); gsub(/"/, "", s); res[nres++] = s
    }
    section == "verbs" && /^  - / {
      s = $0; sub(/^  - /, "", s); gsub(/"/, "", s); vrb[nvrb++] = s
    }

    END {
      for (r = 0; r < nres; r++)
        for (v = 0; v < nvrb; v++)
          print grp "|" res[r] "|" vrb[v]
    }
  ' "$GENERATED" | sort -u
}

# Parse the manager rules block out of _helpers.tpl. The block lives between
# `{{- define "openclaw-operator.managerRules" -}}` and the matching `{{- end }}`,
# plus the cluster-scoped split `openclaw-operator.clusterScopedRules` (#529).
# rbac.yaml renders these rules into ClusterRoles and/or per-namespace Roles via
# `{{ include "openclaw-operator.managerRules" . | nindent 2 }}` (and the
# clusterScopedRules include), so the union of both helpers is the single source
# of truth for what permissions the operator gets.
parse_helm() {
  awk '
    /\{\{-? *define .openclaw-operator\.(managerRules|clusterScopedRules). *-?\}\}/ { in_rules = 1; next }
    in_rules && /\{\{-? *end *-?\}\}/ { if (depth > 0) { depth--; next }; in_rules = 0; next }
    in_rules && /\{\{-? *if / { depth++; next }
    !in_rules { next }
    /^\s*#/ { next }

    /apiGroups:/ {
      s = $0; sub(/.*\[/, "", s); sub(/\].*/, "", s)
      ngroups = split(s, arr, ",")
      for (i = 1; i <= ngroups; i++) {
        g = arr[i]; gsub(/[ "'\''"]/, "", g); groups[i] = g
      }
      next
    }
    /resources:/ {
      s = $0; sub(/.*\[/, "", s); sub(/\].*/, "", s)
      nresources = split(s, arr, ",")
      for (i = 1; i <= nresources; i++) {
        r = arr[i]; gsub(/[ "'\''"]/, "", r); resources[i] = r
      }
      next
    }
    /verbs:/ {
      s = $0; sub(/.*\[/, "", s); sub(/\].*/, "", s)
      nverbs = split(s, arr, ",")
      for (i = 1; i <= nverbs; i++) {
        v = arr[i]; gsub(/[ "'\''"]/, "", v); verbs[i] = v
      }
      # Emit triples for this rule
      for (g = 1; g <= ngroups; g++)
        for (r = 1; r <= nresources; r++)
          for (v = 1; v <= nverbs; v++)
            print groups[g] "|" resources[r] "|" verbs[v]
    }
  ' "$HELPERS" | sort -u
}

# Sanity-check that rbac.yaml actually renders the helper. If someone removes
# the include we want the check to fail loudly rather than silently accept
# matching helper rules that no template references.
if ! grep -q 'openclaw-operator\.managerRules' "$HELM"; then
  echo "::error::$HELM does not include the openclaw-operator.managerRules helper"
  exit 1
fi
if ! grep -q 'openclaw-operator\.clusterScopedRules' "$HELM"; then
  echo "::error::$HELM does not include the openclaw-operator.clusterScopedRules helper (cluster-scoped RBAC, #529)"
  exit 1
fi

GENERATED_TRIPLES=$(parse_generated)
HELM_TRIPLES=$(parse_helm)

# Find triples in generated but not in Helm (i.e., missing permissions)
MISSING=$(comm -23 <(echo "$GENERATED_TRIPLES") <(echo "$HELM_TRIPLES"))

if [ -n "$MISSING" ]; then
  echo "::error::Helm chart RBAC is missing permissions from kubebuilder markers."
  echo ""
  echo "The following (apiGroup | resource | verb) triples are in"
  echo "config/rbac/role.yaml but NOT in the Helm chart ClusterRole:"
  echo ""
  echo "$MISSING" | while IFS='|' read -r g r v; do
    group="$g"
    if [ -z "$group" ]; then group='""'; fi
    echo "  apiGroup=$group  resource=$r  verb=$v"
  done
  echo ""
  echo "Fix: update charts/openclaw-operator/templates/rbac.yaml to match"
  echo "the kubebuilder markers in internal/controller/."
  exit 1
fi

echo "Helm chart RBAC is in sync with kubebuilder markers."
