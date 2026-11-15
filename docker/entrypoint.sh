#!/bin/sh
# Toft container entrypoint.
#
# When BLOCK_AWS_METADATA=1, install egress firewall rules that stop deployed
# app processes from reaching the AWS instance/task metadata endpoints. On ECS,
# 169.254.170.2 hands out the *task IAM role credentials* to anything that asks
# — without this rule every deployed app inherits the task role. 169.254.169.254
# is the EC2 instance metadata service (instance role credentials, user data).
#
# The rules match on the shared primary group of app accounts (APP_USER_GROUP;
# auto-created accounts always get it), so the platform itself — which may
# legitimately need AWS credentials — is unaffected.
#
# Requires CAP_NET_ADMIN. On ECS (EC2 launch type) grant it in the task
# definition:  linuxParameters: { capabilities: { add: ["NET_ADMIN"] } }
# Fargate never grants NET_ADMIN — this is one of the reasons isolation
# requires the EC2 launch type.
set -e

if [ -n "$BLOCK_AWS_METADATA" ]; then
    APP_USER_GROUP="${APP_USER_GROUP:-toft-apps}"
    # The binary would create the group lazily on first deploy; the firewall
    # rule needs it to exist (with a stable GID) before any app runs.
    getent group "$APP_USER_GROUP" >/dev/null || groupadd --system "$APP_USER_GROUP"
    gid="$(getent group "$APP_USER_GROUP" | cut -d: -f3)"

    for dst in 169.254.170.2/32 169.254.169.254/32; do
        if ! iptables -I OUTPUT -d "$dst" -m owner --gid-owner "$gid" -j REJECT; then
            echo "FATAL: BLOCK_AWS_METADATA=1 but the metadata firewall rule could not be" >&2
            echo "installed. The container needs CAP_NET_ADMIN (ECS: linuxParameters" >&2
            echo ".capabilities.add NET_ADMIN). Refusing to start with app processes able" >&2
            echo "to read the task/instance IAM credentials." >&2
            exit 1
        fi
    done
    # IPv6 endpoint of IMDS (fd00:ec2::254); best-effort — not every kernel
    # has ip6tables support and the v4 rules cover the common path.
    ip6tables -I OUTPUT -d fd00:ec2::254/128 -m owner --gid-owner "$gid" -j REJECT 2>/dev/null \
        || echo "notice: ip6tables rule not installed (no IPv6 support?)" >&2
    echo "AWS metadata endpoints blocked for group $APP_USER_GROUP (gid $gid)"
else
    echo "notice: BLOCK_AWS_METADATA is unset — on AWS, deployed apps can read" >&2
    echo "instance/task IAM credentials from the metadata endpoints." >&2
fi

exec /app/toft "$@"
