# Web IDEs

By default, Coder workspaces allow connections via:

- Web terminal
- SSH (plus any [SSH-compatible IDE](../ides.md))

It's common to also let developers to connect via web IDEs.

![Row of IDEs](../images/ide-row.png)

In Coder, web IDEs are defined as
[coder_app](https://registry.terraform.io/providers/coder/coder/latest/docs/resources/app)
resources in the template. With our generic model, any web application can
be used as a Coder application. For example:

```hcl
# Add button to open Portainer in the workspace dashboard
# Note: Portainer must be already running in the workspace
resource "coder_app" "portainer" {
  agent_id      = coder_agent.main.id
  slug          = "portainer"
  display_name  = "Portainer"
  icon          = "https://simpleicons.org/icons/portainer.svg"
  url           = "https://localhost:9443/api/status"

  healthcheck {
    url       = "https://localhost:9443/api/status"
    interval  = 6
    threshold = 10
  }
}
```

## code-server

![code-server in a workspace](../images/code-server-ide.png)

[code-server](https://github.com/coder/coder) is our supported method of running VS Code in the web browser. A simple way to install code-server in Linux/macOS workspaces is via the Coder agent in your template:

```console
# edit your template
cd your-template/
vim main.tf
```

```hcl
resource "coder_agent" "main" {
    arch           = "amd64"
    os             = "linux"
    startup_script = <<EOF
    #!/bin/sh
    # install code-server
    # add '-s -- --version x.x.x' to install a specific code-server version
    curl -fsSL https://code-server.dev/install.sh | sh -s -- --method=standalone --prefix=/tmp/code-server

    # start code-server on a specific port
    # authn is off since the user already authn-ed into the coder deployment
    # & is used to run the process in the background
    /tmp/code-server/bin/code-server --auth none --port 13337 &
    EOF
}
```

For advanced use, we recommend installing code-server in your VM snapshot or container image. Here's a Dockerfile which leverages some special [code-server features](https://coder.com/docs/code-server/):

```Dockerfile
FROM codercom/enterprise-base:ubuntu

# install the latest version
USER root
RUN curl -fsSL https://code-server.dev/install.sh | sh
USER coder

# pre-install VS Code extensions
RUN code-server --install-extension eamodio.gitlens

# directly start code-server with the agent's startup_script (see above),
# or use a process manager like supervisord
```

You'll also need to specify a `coder_app` resource related to the agent. This is how code-server is displayed on the workspace page.

```hcl
resource "coder_app" "code-server" {
  agent_id     = coder_agent.main.id
  slug         = "code-server"
  display_name = "code-server"
  url          = "http://localhost:13337/?folder=/home/coder"
  icon         = "/icon/code.svg"
  subdomain    = true

  healthcheck {
    url       = "http://localhost:13337/healthz"
    interval  = 2
    threshold = 10
  }

}
```

## Microsoft VS Code Server

Microsoft has a VS Code in a browser IDE as well called [VS Code Server](https://code.visualstudio.com/docs/remote/vscode-server) which can be added to a Coder template.

```hcl
resource "coder_agent" "main" {
    arch           = "amd64"
    os             = "linux"
    startup_script = <<EOF
    #!/bin/sh

    # install vs code server
    # alternatively install in a container image Dockerfile
    wget -O- https://aka.ms/install-vscode-server/setup.sh | sh

    # start vs code server
    code-server --accept-server-license-terms serve-local --without-connection-token --quality stable --telemetry-level off >/dev/null 2>&1 &

    EOF
}
```

```hcl
# microsoft vs code server
resource "coder_app" "msft-code-server" {
  agent_id      = coder_agent.main.id
  slug          = "msft-code-server"
  display_name  = "VS Code Server"
  icon          = "/icon/code.svg"
  url           = "http://localhost:8000?folder=/home/coder"
  subdomain     = true
  share         = "owner"

  healthcheck {
    url       = "http://localhost:8000/healthz"
    interval  = 5
    threshold = 15
  }
}
```

## JupyterLab

Configure your agent and `coder_app` like so to use Jupyter. Notice the
`subdomain=true` configuration:

```hcl
data "coder_workspace" "me" {}

resource "coder_agent" "coder" {
  os             = "linux"
  arch           = "amd64"
  dir            = "/home/coder"
  startup_script = <<-EOF
pip3 install jupyterlab
$HOME/.local/bin/jupyter lab --ServerApp.token='' --ip='*'
EOF
}

resource "coder_app" "jupyter" {
  agent_id     = coder_agent.coder.id
  slug         = "jupyter"
  display_name = "JupyterLab"
  url          = "http://localhost:8888"
  icon         = "/icon/jupyter.svg"
  share        = "owner"
  subdomain    = true

  healthcheck {
    url       = "http://localhost:8888/healthz"
    interval  = 5
    threshold = 10
  }
}
```

![JupyterLab in Coder](../images/jupyter-on-docker.png)

## RStudio

Configure your agent and `coder_app` like so to use RStudio. Notice the
`subdomain=true` configuration:

```hcl
resource "coder_agent" "coder" {
  os             = "linux"
  arch           = "amd64"
  dir            = "/home/coder"
  startup_script = <<EOT
#!/bin/bash
# start rstudio
/usr/lib/rstudio-server/bin/rserver --server-daemonize=1 --auth-none=1 &
EOT
}

# rstudio
resource "coder_app" "rstudio" {
  agent_id      = coder_agent.coder.id
  slug          = "rstudio"
  display_name  = "R Studio"
  icon          = "https://upload.wikimedia.org/wikipedia/commons/d/d0/RStudio_logo_flat.svg"
  url           = "http://localhost:8787"
  subdomain     = true
  share         = "owner"

  healthcheck {
    url       = "http://localhost:8787/healthz"
    interval  = 3
    threshold = 10
  }
}
```

![RStudio in Coder](../images/rstudio-port-forward.png)

## Airflow

Configure your agent and `coder_app` like so to use Airflow. Notice the
`subdomain=true` configuration:

```hcl
resource "coder_agent" "coder" {
  os   = "linux"
  arch = "amd64"
  dir  = "/home/coder"
  startup_script = <<EOT
#!/bin/bash
# install and start airflow
pip3 install apache-airflow
/home/coder/.local/bin/airflow standalone &
EOT
}

resource "coder_app" "airflow" {
  agent_id      = coder_agent.coder.id
  slug          = "airflow"
  display_name  = "Airflow"
  icon          = "https://upload.wikimedia.org/wikipedia/commons/d/de/AirflowLogo.png"
  url           = "http://localhost:8080"
  subdomain     = true
  share         = "owner"

  healthcheck {
    url       = "http://localhost:8080/healthz"
    interval  = 10
    threshold = 60
  }
}
```

![Airflow in Coder](../images/airflow-port-forward.png)

## SSH Fallback

If you prefer to run web IDEs in localhost, you can port forward using
[SSH](../ides.md#ssh) or the Coder CLI `port-forward` sub-command. Some web IDEs
may not support URL base path adjustment so port forwarding is the only
approach.
