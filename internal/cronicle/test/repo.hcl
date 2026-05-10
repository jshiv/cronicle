// Fixture for the Repo HCL field test. Asserts that `branch` populates
// Repo.Branch and `commit` populates Repo.Commit (these were crossed in
// an earlier version of the struct tags).
schedule "with_branch" {
  cron = ""
  task "t" {
    command = ["/bin/echo", "hi"]
    repo {
      url    = "https://example.com/x.git"
      branch = "main"
    }
  }
}

schedule "with_commit" {
  cron = ""
  task "t" {
    command = ["/bin/echo", "hi"]
    repo {
      url    = "https://example.com/x.git"
      commit = "abc1234"
    }
  }
}
