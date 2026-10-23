#!/bin/sh
# © Broadcom. All Rights Reserved.
# The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# Overlay for RUN.sysctl, run by vcsim as the container's entrypoint before the
# image's own entrypoint and command, which it execs with the arguments given.
#
# The seed file in $dir holds a line drop-sys-admin=true|false, then key=value
# lines for the emulated keys, with the values the host had. A key the
# container lacks has its parent directory overlaid: a tmpfs of symlinks into a
# bind of the real directory, with seeded regular files for the missing keys.
# A key the container has is a seeded tmpfs file bound over it, or a seeded
# file in the overlay when its directory is overlaid. Writes to the real keys
# still reach the kernel through the symlinks.
#
# The mounts need CAP_SYS_ADMIN. Unless the seed says otherwise, it is dropped
# from the bounding set before exec, so the workload does not keep it.
# Uses POSIX sh and the mount and ln utilities. The drop needs util-linux setpriv.

set -eu

dir=/usr/libexec/vcsim-sysctl
seed=$dir/seed
proc=/proc/sys
work=/run/vcsim-sysctl
n=0

die() {
	echo "vcsim sysctl overlay: $*" >&2
	exit 1
}

# in_seed KEY succeeds if KEY has a seed line.
in_seed() {
	while IFS= read -r line; do
		case $line in
		"$1="*) return 0 ;;
		esac
	done <"$seed"
	return 1
}

# seed_value KEY prints the seed value of KEY, with a newline, as a sysctl read returns it.
seed_value() {
	while IFS= read -r line; do
		case $line in
		"$1="*)
			printf '%s\n' "${line#*=}"
			return 0
			;;
		esac
	done <"$seed"
	die "no seed for $1"
}

# keypath KEY prints the /proc/sys file of a dotted sysctl key.
keypath() {
	printf '%s/%s\n' "$proc" "$(printf '%s' "$1" | tr . /)"
}

# dotted DIR prints the dotted key of a /proc/sys directory.
dotted() {
	printf '%s' "${1#"$proc"/}" | tr / .
}

# overlay_dir DIR replaces the /proc/sys directory DIR with a tmpfs of symlinks
# into a bind of the real directory. Emulated entries are seeded files.
overlay_dir() {
	d=$1
	n=$((n + 1))
	real=$work/real$n
	over=$work/over$n
	mkdir -p "$real" "$over" || die "cannot create $real and $over"
	mount --bind "$d" "$real" || die "cannot bind $d"
	mount -t tmpfs tmpfs "$over" || die "cannot mount tmpfs at $over"

	base=$(dotted "$d")
	for e in "$real"/*; do
		[ -e "$e" ] || continue
		name=${e##*/}
		if in_seed "$base.$name"; then
			seed_value "$base.$name" >"$over/$name"
		else
			ln -s "$e" "$over/$name"
		fi
	done

	# Missing keys of this directory, created as seeded files.
	while IFS= read -r line; do
		case $line in
		drop-sys-admin=* | '') continue ;;
		esac
		k=${line%%=*}
		p=$(keypath "$k")
		if [ "${p%/*}" = "$d" ] && [ ! -e "$p" ]; then
			seed_value "$k" >"$over/${p##*/}"
		fi
	done <"$seed"

	mount --bind "$over" "$d" || die "cannot overlay $d"
	mkdir -p "$work/overlaid"
	: >"$work/overlaid/$base"
}

# bind_file KEY binds a seeded tmpfs file over the existing /proc/sys file of KEY.
bind_file() {
	n=$((n + 1))
	f=$work/file$n
	seed_value "$1" >"$f"
	mount --bind "$f" "$(keypath "$1")" || die "cannot bind $(keypath "$1")"
}

[ -r "$seed" ] || die "no seed at $seed"

mkdir -p "$work" || die "cannot create $work"
mount -t tmpfs tmpfs "$work" || die "cannot mount tmpfs at $work"
mkdir -p "$work/overlaid"

drop=false
while IFS= read -r line; do
	case $line in
	drop-sys-admin=*) drop=${line#drop-sys-admin=} ;;
	esac
done <"$seed"

# Missing keys first: their directories are overlaid, which also covers any
# existing emulated key in them.
while IFS= read -r line; do
	case $line in
	drop-sys-admin=* | '') continue ;;
	esac
	k=${line%%=*}
	p=$(keypath "$k")
	if [ ! -e "$p" ]; then
		d=${p%/*}
		[ -d "$d" ] || die "$k: parent $d does not exist"
		[ -e "$work/overlaid/$(dotted "$d")" ] || overlay_dir "$d"
	fi
done <"$seed"

# Existing keys in directories that were not overlaid.
while IFS= read -r line; do
	case $line in
	drop-sys-admin=* | '') continue ;;
	esac
	k=${line%%=*}
	p=$(keypath "$k")
	if [ -e "$p" ] && [ ! -e "$work/overlaid/$(dotted "${p%/*}")" ]; then
		bind_file "$k"
	fi
done <"$seed"

# setpriv is probed rather than looked up: busybox's has no --bounding-set.
if [ "$drop" = true ]; then
	if setpriv --bounding-set -sys_admin true 2>/dev/null; then
		exec setpriv --bounding-set -sys_admin -- "$@"
	fi
	echo "vcsim sysctl overlay: no setpriv --bounding-set, CAP_SYS_ADMIN kept" >&2
fi
exec "$@"
