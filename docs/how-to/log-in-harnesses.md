# Log in to harnesses

Build the three pinned harness images and sign each CLI in once, inside its own container and volume, so council turns can use your subscription logins. Each login is stored in that harness's own Docker volume. For `claude` and `agy`, whose images carry no vendor software, the login step first installs the pinned tool into a separate tool volume when needed ([harness images and tool install](harness-images.md)).

## Prerequisites

- Docker Engine 28.0 or newer that can build and run `linux/arm64` images (the images are `linux/arm64`): an arm64 host, or emulation you set up yourself.
- The Go toolchain, and a Sablecourt source checkout: the build compiles the council proxy locally.
- Network access from `sablectl` to `downloads.claude.ai` (for `claude`) and `storage.googleapis.com` (for `agy`), for the tool download.
- A way to sign in to each vendor CLI. The sign-in is each vendor CLI's own flow, which Sablecourt forwards to your terminal; the [spike notes](../harnesses.md#harnesses-in-containers) record flows that print an address to open in a browser on the host. Sablecourt does not check that a provider's terms allow a subscription login from a container. For `agy`, read the note on Google's terms in [harness images](harness-images.md#antigravity-terms).

## Steps

1. Build the images from the Sablecourt checkout. Each build also builds the proxy image:

   ```sh
   sablectl harness build claude
   sablectl harness build codex
   sablectl harness build agy
   ```

2. Log in to each harness:

   ```sh
   sablectl harness login claude
   sablectl harness login codex
   sablectl harness login agy
   ```

   For `claude` and `agy` the command first downloads the pinned tool from the vendor, checks its digest and installs it into `sablecourt-harness-<role>-tool`, unless the login volume exists and the installed tool already has the pinned version and digest. It then creates the labelled login volume `sablecourt-harness-<role>` if needed and starts the login in a container with your terminal attached. If the CLI prints a sign-in address, open it in a browser on your host. In the recorded `agy` flow, finish the sign-in, then type `/exit`.

3. After a Sablecourt upgrade (of both `sablectl` and `sablecourtd`) that pins a new tool version, the image tag changes too. Run `sablectl harness build <role>` for each role. For `claude` and `agy`, then run `sablectl harness install <role>`: it replaces the tool and keeps the login. If you skip a step, the daemon refuses the council with the command to run: `sablectl harness build <role>` if the image of the new tag is not present locally, and `sablectl harness install <role>` when preflight reaches that role's tool check and finds another version. The steps per role are in [harness images](harness-images.md#upgrade).

## Check it worked

When it succeeds, the login command ends with `Login for <role> is stored in volume sablecourt-harness-<role>`. Then run a [council](run-a-council.md): the daemon's preflight checks the image, the login volume and the credential file before it creates the council, and for `claude` and `agy` also the tool volume and the tool. The preflight does not ask the provider whether the login is still valid.

## In depth

[Harness images and tool install](harness-images.md), [council harness images and logins](../council-harnesses.md), [harness containers](../how-it-works/harness-containers.md), [harness spike notes](../harnesses.md#harnesses-in-containers).
