variable CI {
  default = false
}
variable GITHUB_REF_NAME {
  default = "local"
}
variable GITHUB_REF_TYPE {
  default = "branch"
}
variable REGISTRY {
  default = "ghcr.io/austindrenski"
}
variable VERSION {
  default = "0.1.10"
}

group default {
  targets = [
    "compose_cf"
  ]
}

target compose_cf {
  cache-from = cache_from(target.compose_cf.labels)
  cache-to   = cache_to(target.compose_cf.labels)
  dockerfile = "Dockerfile"
  labels = {
    "org.opencontainers.image.authors"     = "Austin Drenski <austin@austindrenski.io>"
    "org.opencontainers.image.description" = "Helper tool for legacy Docker Compose Cloud Integrations"
    "org.opencontainers.image.name"        = "compose_cf"
    "org.opencontainers.image.title"       = "Compose -> CloudFormation"
    "org.opencontainers.image.version"     = version()
  }
  output = [
    "type=image"
  ]
  platforms = [
    "linux/amd64",
    "linux/arm64"
  ]
  pull = true
  tags = tags(target.compose_cf.labels)
}

function cache_from {
  params = [
    labels
  ]
  result = [
    format("type=registry,ref=%s/%s:%s-buildkit_cache", REGISTRY, image(labels), "main"),
    format("type=registry,ref=%s/%s:%s-buildkit_cache", REGISTRY, image(labels), github_ref_name())
  ]
}

function cache_to {
  params = [
    labels
  ]
  result = (
    equal(CI, true)
    ?
    [
      format("type=registry,mode=max,ref=%s/%s:%s-buildkit_cache", REGISTRY, image(labels), github_ref_name())
    ]
    :
    []
  )
}

function github_ref_name {
  params = []
  result = lower(replace(GITHUB_REF_NAME, "/", "-"))
}

function image {
  params = [
    labels
  ]
  result = labels["org.opencontainers.image.name"]
}

function tags {
  params = [
    labels
  ]
  result = (
    equal(version(), VERSION)
    ?
    [
      format("%s/%s:%s", REGISTRY, image(labels), "latest"),
      format("%s/%s:%s", REGISTRY, image(labels), version())
    ]
    :
    [
      format("%s/%s:%s", REGISTRY, image(labels), version())
    ]
  )
}

function version {
  params = []
  result = (
    equal(GITHUB_REF_TYPE, "tag")
    ?
    trimprefix(github_ref_name(), "v")
    :
    format("%s-ci.%s", VERSION, github_ref_name())
  )
}
