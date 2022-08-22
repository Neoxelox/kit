import superinvoke

from .tags import Tags


class Tools(superinvoke.Tools):
    Go = superinvoke.Tool(
        name="go",
        version="1.19",
        tags=[Tags.ALL],
        path="go",
    )

    Git = superinvoke.Tool(
        name="git",
        version="2.34.1",
        tags=[Tags.ALL],
        path="git",
    )

    Curl = superinvoke.Tool(
        name="curl",
        version="7.81.0",
        tags=[Tags.ALL],
        path="curl",
    )

    Test = superinvoke.Tool(
        name="gotestsum",
        version="1.8.2",
        tags=[Tags.DEV, Tags.CI_INT],
        links={
            superinvoke.Platforms.LINUX: (
                "https://github.com/gotestyourself/gotestsum/releases/download/v1.8.2/gotestsum_1.8.2_linux_amd64.tar.gz",
                "gotestsum",
            ),
        },
    )

    Lint = superinvoke.Tool(
        name="golangci-lint",
        version="1.48.0",
        tags=[Tags.DEV, Tags.CI_INT],
        links={
            superinvoke.Platforms.LINUX: (
                "https://github.com/golangci/golangci-lint/releases/download/v1.48.0/golangci-lint-1.48.0-linux-amd64.tar.gz",
                "golangci-lint-1.48.0-linux-amd64/golangci-lint",
            ),
        },
    )
