"""Resolve an immutable Server tag or a manual develop Beta request."""
import os
import re
import subprocess


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def classify(event, ref, on_main, on_develop):
    if event == "workflow_dispatch":
        if ref != "refs/heads/develop" or not on_develop:
            raise ValueError("Manual Beta must use current develop")
        return "beta"
    if event != "push" or not re.fullmatch(r"refs/tags/server-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", ref):
        raise ValueError("Only namespaced Server version tag pushes are supported")
    if on_main:
        return "stable"
    if on_develop:
        return "beta"
    raise ValueError("Tag must belong to main or develop")


def main():
    event, ref = os.environ["GITHUB_EVENT_NAME"], os.environ["GITHUB_REF"]
    git("fetch", "--force", "origin", "+refs/heads/main:refs/remotes/origin/main", "+refs/heads/develop:refs/remotes/origin/develop")
    commit = git("rev-parse", "HEAD")
    on_main = subprocess.run(["git", "merge-base", "--is-ancestor", commit, "origin/main"]).returncode == 0
    on_develop = subprocess.run(["git", "merge-base", "--is-ancestor", commit, "origin/develop"]).returncode == 0
    if event == "workflow_dispatch":
        on_develop = commit == git("rev-parse", "origin/develop")
    channel = classify(event, ref, on_main, on_develop)
    raw = ref.removeprefix("refs/tags/server-v") if event == "push" else os.environ["WORKFLOW_INPUT_VERSION"].strip().removeprefix("v")
    if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", raw):
        raise ValueError("Invalid Server version")
    print(f"version={raw}\ntag_name=server-v{raw}\nchannel={channel}\ncommit={commit}")


if __name__ == "__main__":
    main()
