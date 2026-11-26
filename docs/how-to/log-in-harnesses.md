# Log in to harnesses

Build the three pinned harness images and sign each CLI in once, inside its own container and volume, so council turns can use your subscription logins. Each login is stored in that harness's own Docker volume. For `claude` and `agy`, whose images carry no vendor software, the login step first installs the pinned tool into a separate tool volume ([harness images and tool install](harness-images.md)).

## Prerequisites

- Docker Engine 28.0 or newer, on an arm64 host (the images are `linux/arm64`).
- The Go toolchain, and a Sablecourt source checkout: the build compiles the council proxy locally.
- Network access from `sablectl` to `downloads.claude.ai` (for `claude`) and `storage.googleapis.com` (for `agy`), for the tool download.
- An account for each provider, and a browser on the host for the sign-in page. Sablecourt does not check that a provider's terms allow a subscription login from a container. For `agy`, read the note on Google's terms in [harness images](harness-images.md#antigravity-terms).

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

   For `claude` and `agy` the command first downloads the pinned tool from the vendor, checks its digest and installs it into `sablecourt-harness-<role>-tool`, unless the right version is already there. It then creates the labelled login volume `sablecourt-harness-<role>` if needed and starts the login in a container with your terminal attached. Open the printed address in your host browser. For `agy`, finish the sign-in, then type `/exit`.

3. After a Sablecourt upgrade that pins a new tool version, a council is refused with `sablectl harness install <role>`. Run that command: it replaces the tool and keeps the login.

## Check it worked

The login command ends with `Login for <role> is stored in volume sablecourt-harness-<role>`. Then run a [council](run-a-council.md): the daemon's preflight checks the image, both volumes, the tool and the credential file before it creates the council. Nothing in Sablecourt checks that a login is still valid with the provider before a turn starts.

## In depth

[Harness images and tool install](harness-images.md), [council harness images and logins](../council-harnesses.md), [harness containers](../how-it-works/harness-containers.md), [harness spike notes](../harnesses.md#harnesses-in-containers).
