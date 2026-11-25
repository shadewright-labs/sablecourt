# Log in to harnesses

Status: partly built. Building images is a command; the login is manual until a `sablectl harness login` command exists (#79).

Build the three pinned harness images and sign each CLI in once, inside its own container and volume, so council turns can use your subscription logins without any credential leaving Docker.

## Prerequisites

- Docker Engine 28.0 or newer, on an arm64 host (the images are `linux/arm64`).
- The Go toolchain, and a Sablecourt source checkout: the build compiles the council proxy locally.
- An account for each provider, and a browser on the host for the sign-in page. Sablecourt does not check that your provider's terms allow a subscription login from a container.

## Steps

1. Build the images from the Sablecourt checkout. Each build also builds the proxy image:

   ```sh
   sablectl harness build claude
   sablectl harness build codex
   sablectl harness build agy
   ```

2. Find the image tag the daemon expects, one per harness:

   ```sh
   docker image ls 'sablecourt-harness-*'
   ```

3. Create the labelled login volume for each harness. The daemon refuses a volume without these labels:

   ```sh
   docker volume create \
     --label app.kubernetes.io/managed-by=sablecourt \
     --label sablecourt.io/harness=claude \
     sablecourt-harness-claude
   ```

   Repeat with `codex` and `agy` in both the label and the name.
4. Run the login for each harness in a container shaped like a council turn. For `claude` (use the tag from step 2):

   ```sh
   docker run --rm -it --user 10001:10001 --read-only --cap-drop ALL \
     --security-opt no-new-privileges \
     --tmpfs /scratch:rw,noexec,nosuid,nodev,mode=0700,uid=10001,gid=10001 \
     -e TMPDIR=/scratch -v sablecourt-harness-claude:/home/harness \
     sablecourt-harness-claude:<tag> claude auth login
   ```

   For `codex` the command is `codex login --device-auth`. For `agy` it is `agy` alone: finish the sign-in, then type `/exit`. Open the printed URL in your host browser and follow the prompts.

## Check it worked

Look for the credential file in the volume (names from the container spike): `.claude/.credentials.json` for `claude`, `.codex/auth.json` for `codex`, files under `.gemini/` for `agy`:

```sh
docker run --rm --user 10001:10001 --read-only -v sablecourt-harness-codex:/home/harness \
  sablecourt-harness-codex:<tag> ls -a /home/harness/.codex
```

Then run a [council](run-a-council.md). An expired or missing login is not caught early: the turn fails inside the harness with its own message.

## Not built yet

Today nothing checks logins before a round. The decision recorded on the open issue is that a refused round will name the harness and the command to run, that `sablectl harness login <harness>` will create the labelled volume and start the login through Docker with a terminal attached, and that "login present" means credential files exist in the volume, read in a no-network container, without a live provider call. An expired login would still fail at the turn.

## In depth

[Harness containers](../how-it-works/harness-containers.md), [harness spike notes](../harnesses.md#harnesses-in-containers), [council](../how-it-works/council.md).
