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

VERSION_TO_TAG=$(sed -n 'N;N;s#^variable VERSION {\n  default = "\(.*\)"\n}$#\1#p' docker-bake.hcl)

git tag --sign -m "Version $VERSION_TO_TAG" "v$VERSION_TO_TAG"

sed -i "N;N;s#^\(variable VERSION {\n  default = \"\)\(.*\)\(\".*\)#\1$VERSION\3#" docker-bake.hcl

git add docker-bake.hcl
git commit -m "Bump version to $VERSION"
