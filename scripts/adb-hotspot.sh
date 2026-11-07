#!/usr/bin/env bash
# Connect adb to an Android phone over its hotspot, without looking up the IP.
#
# Usage: scripts/adb-hotspot.sh [serial]
#   serial  USB serial of the phone (default: $PHONE_SERIAL, else the one phone on USB)
#
# Needs the phone plugged in over USB with USB debugging enabled. Reads the
# phone's hotspot address from its hotspot interface, switches adbd to TCP on
# port 5555, then connects over the network. Unplug the cable afterwards.
set -euo pipefail

SERIAL="${1:-${PHONE_SERIAL:-}}"
IFACE="${IFACE:-swlan0}"
PORT="${PORT:-5555}"
ADB="${ADB:-$(command -v adb || true)}"
ADB="${ADB:-$HOME/Library/Android/sdk/platform-tools/adb}"

if [[ ! -x "$ADB" ]]; then
	echo "adb not found; set ADB=/path/to/adb" >&2
	exit 1
fi

if [[ -n "$SERIAL" ]]; then TARGET=(-s "$SERIAL"); else TARGET=(-d); SERIAL="on USB"; fi

if ! "$ADB" "${TARGET[@]}" get-state >/dev/null 2>&1; then
	echo "Phone $SERIAL is not connected over USB." >&2
	exit 1
fi

# Read the hotspot address before switching to TCP, since the USB transport
# goes away when adbd restarts.
IP="$("$ADB" "${TARGET[@]}" shell ip -4 addr show "$IFACE" 2>/dev/null \
	| tr -d '\r' \
	| awk '/inet /{split($2, a, "/"); print a[1]; exit}')"

if [[ -z "$IP" ]]; then
	echo "No IPv4 address on $IFACE. Is the hotspot on?" >&2
	exit 1
fi

echo "Hotspot address: $IP"
"$ADB" "${TARGET[@]}" tcpip "$PORT"
sleep 2

"$ADB" disconnect "$IP:$PORT" >/dev/null 2>&1 || true
"$ADB" connect "$IP:$PORT"
"$ADB" devices -l
