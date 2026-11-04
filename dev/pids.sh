# Gemeinsame Hilfsfunktionen für run-all.sh und stop-all.sh (wird gesourced).
# Erwartet die Variable $bin (dev/bin).

# is_ours <pid> <binary-name>: läuft unter PID genau $bin/<binary-name>?
is_ours() {
  local cmd
  cmd="$(ps -p "$1" -o command= 2>/dev/null)" || return 1
  [[ "$cmd" == "$bin/$2" || "$cmd" == "$bin/$2 "* ]]
}

# any_running <pidfile>: gibt Name und PID des ersten noch laufenden eigenen Prozesses aus, Status 0 wenn einer läuft.
any_running() {
  local pid name
  [[ -f "$1" ]] || return 1
  while read -r pid name; do
    [[ -n "$pid" && -n "${name:-}" ]] || continue
    if is_ours "$pid" "$name"; then
      echo "$name $pid"
      return 0
    fi
  done <"$1"
  return 1
}

# stop_recorded <pidfile>: beendet nur eigene Prozesse; alles andere wird übersprungen.
stop_recorded() {
  local pid name
  [[ -f "$1" ]] || return 0
  while read -r pid name; do
    [[ -n "$pid" ]] || continue
    if [[ -z "${name:-}" ]]; then
      echo "Warnung: PID $pid ohne Namen (altes Format), wird nicht beendet."
    elif is_ours "$pid" "$name"; then
      kill "$pid" 2>/dev/null || true
    else
      echo "Überspringe PID $pid ($name): kein Prozess aus $bin/$name."
    fi
  done <"$1"
  rm -f "$1"
}
