#!/usr/bin/env bash
set -euo pipefail

BACKUP_FILE="${1:-/tmp/maxfreq.before}"

if [[ "${EUID}" -ne 0 ]]; then
  echo "run as root: sudo $0 [backup-file]" >&2
  exit 1
fi

restored=0

if [[ -f "${BACKUP_FILE}" ]]; then
  while read -r file value; do
    [[ -n "${file:-}" && -n "${value:-}" ]] || continue
    [[ -f "$file" ]] || continue
    echo "$value" > "$file"
    restored=$((restored + 1))
  done < "$BACKUP_FILE"
  echo "restored ${restored} scaling_max_freq entries from ${BACKUP_FILE}"
  exit 0
fi

for maxf in /sys/devices/system/cpu/cpu*/cpufreq/scaling_max_freq /sys/devices/system/cpu/cpufreq/policy*/scaling_max_freq; do
  [[ -f "$maxf" ]] || continue
  cpudir="$(dirname "$maxf")"
  info_max="${cpudir}/cpuinfo_max_freq"
  [[ -f "$info_max" ]] || continue
  cat "$info_max" > "$maxf"
  restored=$((restored + 1))
done

echo "restored ${restored} scaling_max_freq entries from cpuinfo_max_freq defaults"
