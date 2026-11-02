#!/bin/sh
# Usage: release.sh preflight|publish|dry-run vX.Y.Z
# preflight checks that a release may start; dry-run builds every artifact into dist/<version> and stops;
# publish pushes the image, tags, then creates the GitHub release. Every publish step is skipped when
# already done, so a failed publish is finished by running it again.
set -eu

mode=${1:?mode}
tag=${2:?version}
REGISTRY=${REGISTRY:-ghcr.io/sarpaydemir}
TARGETS="linux/amd64 linux/arm64 darwin/arm64"
PLATFORMS=linux/amd64,linux/arm64
BUILDER=melinda-release
GO_IMAGE=$(grep -m1 -o 'golang:[^ ]*' Dockerfile)
image=$REGISTRY/melinda

die() { echo "release: $*" >&2; exit 1; }

printf '%s' "$tag" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$' ||
	die "VERSION must be semver like v1.2.3 or v1.2.3-rc.1"
case $tag in *-*) pre=1 ;; *) pre= ;; esac
version=${tag#v}

remote_tag_commit() { git ls-remote origin "refs/tags/$tag^{}" "refs/tags/$tag" | awk 'END { print $1 }'; }

case $mode in
preflight | publish)
	[ -z "$(git status --porcelain)" ] || die "working tree is not clean"
	[ "$(git rev-parse --abbrev-ref HEAD)" = main ] || die "not on main"
	git fetch --quiet origin main
	[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || die "main does not match origin/main"
	gh auth status >/dev/null 2>&1 || die "gh is not authenticated (gh auth login)"
	grep -qs "${REGISTRY%%/*}" "${DOCKER_CONFIG:-$HOME/.docker}/config.json" ||
		die "not logged in to ${REGISTRY%%/*} (docker login)"
	rc=$(remote_tag_commit)
	[ -z "$rc" ] || [ "$rc" = "$(git rev-parse HEAD)" ] || die "tag $tag exists on origin at another commit"
	[ "$mode" = preflight ] && exit 0
	;;
dry-run) ;;
*) die "unknown mode $mode" ;;
esac

commit=$(git rev-parse --short=12 HEAD)
epoch=$(git log -1 --format=%ct)
date=$(date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%SZ)
ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.date=$date"
dist=dist/$version
cache=$PWD/dist/.cache

rm -rf "$dist"
mkdir -p "$dist" "$cache/mod" "$cache/build"

build() { # os arch out gocache; same toolchain as the image
	docker run --rm -u "$(id -u):$(id -g)" -v "$PWD:/src" -w /src \
		-v "$cache/mod:/gomod" -v "$4:/gocache" \
		-e CGO_ENABLED=0 -e "GOOS=$1" -e "GOARCH=$2" -e GOMODCACHE=/gomod -e GOCACHE=/gocache \
		-e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local -e HOME=/tmp \
		"$GO_IMAGE" go build -trimpath -ldflags "$ldflags" -o "$3" ./cmd/melinda
}

for t in $TARGETS; do
	os=${t%/*}
	arch=${t#*/}
	name=melinda_${version}_${os}_${arch}
	stage=$dist/.stage/$name
	mkdir -p "$stage"
	build "$os" "$arch" "$stage/melinda" "$cache/build"
	cp README.md LICENSE "$stage/"
	chmod 755 "$dist/.stage" "$stage" "$stage/melinda"
	chmod 644 "$stage/README.md" "$stage/LICENSE"
	tar -C "$dist/.stage" --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" -cf - "$name" |
		gzip -9n >"$dist/$name.tar.gz"
done

# rebuild with an empty build cache: the result must be byte-identical
fresh=$(mktemp -d "$PWD/dist/.fresh.XXXXXX")
build linux amd64 "$dist/.repro" "$fresh"
rm -rf "$fresh"
cmp -s "$dist/.repro" "$dist/.stage/melinda_${version}_linux_amd64/melinda" || die "build is not reproducible"
rm -rf "$dist/.repro" "$dist/.stage"

(cd "$dist" && sha256sum melinda_*.tar.gz >SHA256SUMS)

section() {
	[ -f CHANGELOG.md ] && awk -v v="$version" '
		/^## / { on = ($0 ~ "^## \\[?v?" v "\\]?([ ]|$)"); next }
		on { print }' CHANGELOG.md
}
if section | grep -q '[^[:space:]]'; then
	section >"$dist/NOTES.md"
else
	prev=$(git tag --list 'v*' --sort=-v:refname | grep -vx "$tag" | { [ -n "$pre" ] && cat || grep -v -- '-'; } | head -n1 || true)
	git log --no-merges --format='- %s' ${prev:+"$prev..HEAD"} >"$dist/NOTES.md"
fi

buildx() {
	docker buildx inspect "$BUILDER" >/dev/null 2>&1 ||
		docker buildx create --name "$BUILDER" --driver docker-container >/dev/null
	docker buildx build --builder "$BUILDER" --platform "$PLATFORMS" \
		--build-arg "VERSION=$version" --build-arg "COMMIT=$commit" --build-arg "DATE=$date" \
		--provenance=false "$@" .
}

if [ "$mode" = dry-run ]; then
	buildx -t "$image:$version" --output "type=oci,dest=$dist/melinda_${version}_image.oci.tar"
	echo "dry run complete; nothing tagged, pushed, or published"
	ls -l "$dist"
	exit 0
fi

if docker buildx imagetools inspect "$image:$version" >/dev/null 2>&1; then
	echo "image $image:$version already pushed"
else
	buildx -t "$image:$version" --push
fi
if [ -z "$pre" ]; then
	docker buildx imagetools create -t "$image:latest" "$image:$version"
fi

if [ -n "$(remote_tag_commit)" ]; then
	echo "tag $tag already on origin"
else
	git rev-parse -q --verify "refs/tags/$tag" >/dev/null || git tag -a "$tag" -m "melinda $version"
	git push origin "refs/tags/$tag" || { git tag -d "$tag" >/dev/null; die "tag push failed"; }
fi

if gh release view "$tag" >/dev/null 2>&1; then
	echo "release $tag already exists"
else
	gh release create "$tag" "$dist"/melinda_*.tar.gz "$dist/SHA256SUMS" \
		--title "melinda $version" --notes-file "$dist/NOTES.md" --verify-tag ${pre:+--prerelease} ||
		die "release creation failed; image and tag are published, run: make release VERSION=$tag"
fi
ls -l "$dist"
