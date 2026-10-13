#!/usr/bin/env bash
#
# reload.sh - rebuild one component from the working tree and roll it out.
#
# Usage: scripts/simcluster/reload.sh <agent|controller-manager|scheduler|simulator|all> [--force]
#
# Rebuilds the binaries, imports the component's image when it changed,
# re-applies the CRDs when they changed, and reruns deploy.sh's helm command
# for the component's release: the values files deploy.env records and the
# image tags of images.env, where only the component's tag changed. The
# charts and those values files are read again, as a deploy would. A tag
# follows the binary's content, so an unchanged binary changes nothing: no
# import, no upgrade, no restart. The other components keep their
# images.env entries and their pods, even when shared code changed their
# binaries too; reload them, or all, to roll those out.
# --force (or SIMCLUSTER_FORCE_RELOAD=1) restarts the component anyway, by
# setting the pod template annotation sim.joulie.io/reload to the current
# time; touching a source file is not enough, since the binary stays the
# same. Prints the seconds the reload took.
#
# The component may also come from COMPONENT or C.
#
set -euo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

component=
force=${SIMCLUSTER_FORCE_RELOAD:-0}
while (($#)); do
  case $1 in
    --force)
      force=1
      shift
      ;;
    -h | --help)
      sim_usage "$0"
      exit 0
      ;;
    -*) sim_die "unknown argument $1 (see --help)" ;;
    *)
      [[ -z $component ]] || sim_die "one component only (see --help)"
      component=$1
      shift
      ;;
  esac
done
component=${component:-${COMPONENT:-${C:-}}}
case $component in
  agent | controller-manager | scheduler | simulator) components=("$component") ;;
  all) components=(agent controller-manager scheduler simulator) ;;
  *) sim_die "component '${component}' is not agent, controller-manager, scheduler, simulator or all" ;;
esac
[[ $force == 1 ]] || force=0

sim_init
sim_require docker kubectl helm jq sha256sum
sim_require_cluster
start=$SECONDS

# The values files of the last deploy, comma separated in deploy.env.
deploy_env=$SIMCLUSTER_DIR/deploy.env
[[ -s $deploy_env ]] || sim_die "$deploy_env is missing; run make simcluster-deploy first"
IFS=, read -ra joulie_files <<<"$(sed -n 's/^JOULIE_VALUES=//p' "$deploy_env")"
IFS=, read -ra sim_files <<<"$(sed -n 's/^SIM_VALUES=//p' "$deploy_env")"
((${#joulie_files[@]} && ${#sim_files[@]})) || sim_die "$deploy_env lists no JOULIE_VALUES or SIM_VALUES; run make simcluster-deploy"
for f in "${joulie_files[@]}" "${sim_files[@]}"; do
  [[ -e $f ]] || sim_die "$f, a values file of the last deploy, is missing; run make simcluster-deploy"
done

bash "$SIM_SCRIPTS/images.sh" --joulie "${components[@]}"
sim_load_images
if [[ $(sim_crds_sha) != "$(cat "$SIMCLUSTER_DIR/crds.sha" 2>/dev/null || true)" ]]; then
  sim_log "the CRDs changed: applying them"
  sim_apply_crds
fi

# Per release its components; per component its namespace, image tag key
# and new image.
members() { [[ $1 == "$SIM_RELEASE_SIM" ]] && echo simulator || echo agent controller-manager scheduler; }
namespace() { [[ $1 == simulator ]] && echo "$SIM_NS_SIM" || echo "$SIM_NS_JOULIE"; }
asked() { [[ " ${components[*]} " == *" $1 "* ]]; }
tag_key() {
  case $1 in
    agent) echo agent.image.tag ;;
    controller-manager) echo controllerManager.image.tag ;;
    scheduler) echo schedulerExtender.image.tag ;;
    simulator) echo image.tag ;;
  esac
}
new_image() {
  case $1 in
    agent) echo "$IMAGE_AGENT" ;;
    controller-manager) echo "$IMAGE_CONTROLLER_MANAGER" ;;
    scheduler) echo "$IMAGE_SCHEDULER" ;;
    simulator) echo "$IMAGE_SIMULATOR" ;;
  esac
}
# The workloads that run a component's image.
workloads() {
  case $1 in
    agent)
      kubectl -n "$SIM_NS_JOULIE" get deployments,daemonsets,statefulsets -o json |
        jq -r '.items[] | select(.spec.template.metadata.labels["app.kubernetes.io/name"] == "joulie-agent") | "\(.kind | ascii_downcase)/\(.metadata.name)"'
      ;;
    controller-manager) echo deployment/joulie-controller-manager ;;
    scheduler) echo deployment/joulie-scheduler-extender ;;
    simulator) echo "deployment/$SIM_SIM_DEPLOYMENT" ;;
  esac
}

# A release is upgraded when one of its tags in images.env is not the one it
# runs. That is the asked component's, unless images.sh was run by hand
# since: then the release's other changed components roll out too.
restart=()
for rel in "$SIM_RELEASE_JOULIE" "$SIM_RELEASE_SIM"; do
  mine=$(members "$rel")
  wanted=0
  for c in $mine; do asked "$c" && wanted=1; done
  ((wanted)) || continue
  current=$(helm get values "$rel" -n "$(namespace "${mine%% *}")" -o json)
  changed=0
  for c in $mine; do
    key=$(tag_key "$c")
    tag=$(sim_tag "$(new_image "$c")")
    have=$(jq -r --arg k "$key" 'getpath($k | split(".")) // empty' <<<"$current")
    if [[ $have != "$tag" ]]; then
      sim_log "$c: ${have:-unset} -> $tag"
      restart+=("$c")
      changed=1
    elif ((force)) && asked "$c"; then
      restart+=("$c")
    fi
  done
  if ((changed)); then
    if [[ $rel == "$SIM_RELEASE_SIM" ]]; then
      sim_helm_sim "${sim_files[@]}"
    else
      sim_helm_joulie "${joulie_files[@]}"
    fi
  fi
done

if ((${#restart[@]} == 0)); then
  sim_log "nothing to reload: ${components[*]} already run the images of this tree (--force restarts anyway)"
  exit 0
fi

stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
for c in "${restart[@]}"; do
  ns=$(namespace "$c")
  for obj in $(workloads "$c"); do
    if ((force)); then
      kubectl -n "$ns" patch "$obj" --type=merge \
        -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"sim.joulie.io/reload\":\"$stamp\"}}}}}" >/dev/null
    fi
    kubectl -n "$ns" rollout status "$obj" --timeout=300s >/dev/null
  done
done
sim_verify_images
sim_log "reloaded ${restart[*]} in $((SECONDS - start)) s"
