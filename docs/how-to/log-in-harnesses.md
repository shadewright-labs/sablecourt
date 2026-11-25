# Log in to harnesses

Status: partly built. Building images is a command; the login is manual until `sablectl harness login` exists (#79, not built yet).

Build the three pinned harness images and sign each CLI in once, inside its own container and volume, so council turns can use your subscription logins without any credential leaving Docker. `sablectl harness login <harness>` will replace the manual steps below: it will create the labelled volume and run the login for you.

## Prerequisites

- Docker Engine 28.0 or newer, on an arm64 host (the images are `linux/arm64`).
- The Go toolchain, and a Sablecourt source checkout: the build compiles the council proxy locally.
- An account for each provider, and a browser on the host for the sign-in page. Sablecourt does not check that a provider's terms allow a subscription login from a container.

## Steps

1. Build the images from the Sablecourt checkout. Each build also builds the proxy image, and the last line names the tag to use below:

   ```sh
   sablectl harness build claude
   sablectl harness build codex
   sablectl harness build agy
   ```

2. Create the labelled login volume for each harness. The daemon refuses a volume without both labels, and every harness needs its own volume. The name `sablecourt-harness-<role>` is the one the daemon looks for:

   ```sh
   docker volume create \
     --label app.kubernetes.io/managed-by=sablecourt \
     --label sablecourt.io/harness=claude \
     sablecourt-harness-claude
   ```

   Repeat with `codex` and `agy` in the label and the name.
3. Check that a container shaped like a council turn starts and sees no login yet. Use the tag from step 1:

   ```sh
   docker run --rm --user 10001:10001 --read-only --cap-drop ALL \
     --security-opt no-new-privileges \
     --tmpfs /scratch:rw,noexec,nosuid,nodev,mode=0700,uid=10001,gid=10001 \
     -e TMPDIR=/scratch -v sablecourt-harness-claude:/home/harness \
     sablecourt-harness-claude:<tag> claude auth status
   ```

   Before a login it prints `"loggedIn": false`. For `codex` the end of the command is `codex login status` (prints `Not logged in`, exit code 1). `agy` has no status command; `agy --version` only proves the container starts.
4. Sign in by running the same command with `-it` added after `--rm` and the login command in place of the status command: `claude auth login`, `codex login --device-auth`, or `agy` alone (finish the sign-in, then type `/exit`). Open the printed address in your host browser. This step needs an account, so it was not run while writing this page.

## Check it worked

Run the step 3 command again. `claude` should report `"loggedIn": true` and `codex` should print that you are logged in. Then run a [council](run-a-council.md). An expired login is not caught early: the turn fails inside the harness with its own message.

## Not built yet

Nothing checks logins before a round (#79). The decision on that issue: a refused round will name the harness and the command to run, `sablectl harness login <harness>` will create the volume with its label and start the login through Docker with a terminal attached, and "login present" will mean that credential files exist in the volume, read from a container with no network, with no live call to the provider. An expired login would still fail at the turn.

## In depth

[Harness containers](../how-it-works/harness-containers.md), [harness spike notes](../harnesses.md#harnesses-in-containers), [council](../how-it-works/council.md).
