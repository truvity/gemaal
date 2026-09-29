#!/usr/bin/env bash
# This repository is public and its history cannot be unpublished — a
# rewrite changes the SHAs but not what was already fetched. So the rule
# ("mechanism only; particulars are caller inputs or org variables") is
# enforced mechanically rather than remembered.
#
# Copied from truvity/ci-workflows, which is public for the same reason.
# Add a pattern here the first time something new turns out to be a
# particular. Never add an exception without one.
set -uo pipefail

# The 12-digit patterns are anchored on word boundaries. Without them,
# `[0-9]{12}` also matches a 12-digit run that happens to fall inside a
# longer hex string -- and a nixpkgs commit SHA is exactly that. The
# devbox bump to 17de0b976395537756f30a3e78f2f06e5cec89ed contains
# `976395537756`, which failed this canary simultaneously in every repo
# that carries it, for a value that is neither a particular nor secret.
# `\b` keeps every real shape (bare, in an ARN, as an ECR host: each is
# bounded by a non-word character) and drops the hex-embedded ones.
patterns=(
  '\b[0-9]{12}\b'                          # AWS account id
  'arn:aws'                            # any ARN
  '\b[0-9]{12}\.dkr\.ecr\.'              # ECR registry host
  '\.svc\.cluster\.local'              # in-cluster DNS
  '/secrets/[a-z0-9-]+/'               # SSM parameter paths (/secrets/<system>/<name>; a
                                       # bare /secrets/<key> mount path — e.g. the standard
                                       # kubernetes.io service-account token mount — is
                                       # Kubernetes' own mechanism, not a particular)
  'truvity-[a-z0-9-]*-(ci-cache|artifacts|state)'   # S3 buckets
  '\.truvity\.(xyz|com|co)'            # internal hostnames
  'glpat-|ghp_|github_pat_'            # tokens, in case of an accident
)

fail=0

# Scan TRACKED FILES ONLY. The point of this canary is to stop particulars
# being committed, so git's index is exactly the right scope -- and a
# recursive walk of the working tree is not. It descended into generated,
# gitignored directories: .devbox/state.json carries a
# `nix_print_dev_env_hash` whose hex contains a 12-digit run, which matched
# the AWS-account-id pattern. That made the canary fail on a clean checkout
# for a value that is neither committed nor secret.
#
# This matters more than a nuisance: a canary that cries wolf is one people
# learn to skip, and this one is what stands between us and publishing
# particulars from a public repo.
mapfile -d '' tracked < <(git ls-files -z)

for p in "${patterns[@]}"; do
  # Exclude this script: it necessarily contains the patterns it bans.
  #
  # Two more exclusions, both of KNOWN-SAFE shapes rather than this
  # repository's real particulars:
  #   - 123456789012 / 123456789013 are the two placeholder AWS account
  #     ids this repository's own examples and pkg/identity's ARN-parsing
  #     test fixtures use (123456789012 is AWS's own documented example
  #     account id, reused throughout their public docs). pkg/identity
  #     parses ARN strings for a living, so its tests need ARN-shaped
  #     fixtures — a placeholder that is not digit-shaped would test a
  #     different, easier parse than production input actually has.
  #   - /secrets/prod/ and /secrets/mirror/key, in pkg/config's and
  #     pkg/engine's own test suites, illustrate the /secrets refusal
  #     rule with a generic, non-estate-specific name — the same role
  #     "truvity-prod-data" plays for the S3 bucket rule three lines
  #     below, which the bucket pattern already does not match.
  if hits=$(printf '%s\0' "${tracked[@]}" \
              | grep -zZv '^hack/leak-canary\.sh$' \
              | xargs -0 -r grep -InE "$p" 2>/dev/null \
              | grep -vE '123456789012|123456789013' \
              | grep -vF '/secrets/prod/' \
              | grep -vF '/secrets/mirror/key'); then
    echo "LEAK: pattern /$p/ matched — particulars belong in caller inputs or org variables:"
    echo "$hits" | head -5 | sed 's/^/    /'
    fail=1
  fi
done

if [ "$fail" = 0 ]; then
  echo "leak canary clean — ${#patterns[@]} patterns checked, no particulars found"
fi
exit $fail
