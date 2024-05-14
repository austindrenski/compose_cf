#!/usr/bin/env sh

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <version>"
  exit 1
fi

VERSION=$1

case $VERSION in
*-)
  echo "Version must not end with '-'" >&2
  exit 1
  ;;
esac

VERSION_TO_TAG=$(sed -n 's#^variable VERSION { default = "\(.*\)" }$#\1#p' docker-bake.hcl)

git tag --sign -m "Version $VERSION_TO_TAG" "v$VERSION_TO_TAG"

sed -i "s#^variable VERSION { default = .*}\$#variable VERSION { default = \"$VERSION\" }#" docker-bake.hcl

git add docker-bake.hcl
git commit -m "Bump version to $VERSION"
