// cronicle.hcl: schedule as code.
//
// Try it:
//   cronicle exec --path cronicle.hcl                 # run every schedule once, now
//   cronicle exec --path cronicle.hcl --task numbers  # run one task (not its depends)
//   cronicle run  --path cronicle.hcl                 # start the scheduler
//
// Keep each task one small, obvious step whose whole command is written
// out here, and build bigger jobs by chaining tasks with `depends`. Every
// run then shows exactly what ran, in what order, and which step failed.
// See AGENTS.md (or `cronicle guide`) for the full guide.

schedule "example" {
  // Cron expression, "@every 1m", "@once", or "" (only run with exec).
  cron = "@every 1m"

  // ${scratch} is a directory shared by every task in one run of this
  // schedule: the place to hand files from one step to the next.
  task "numbers" {
    command = ["sh", "-c", "seq 1 10 > ${scratch}/numbers.txt && echo 'wrote 10 numbers'"]
  }

  // `numbers` must succeed first. `total` and `evens` share that one
  // dependency, so they run in parallel.
  task "total" {
    depends = ["numbers"]
    command = ["python3", "-c", <<-EOT
      nums = [int(n) for n in open("${scratch}/numbers.txt")]
      print("total", sum(nums))
      open("${scratch}/total.txt", "w").write(str(sum(nums)))
    EOT
    ]
  }

  task "evens" {
    depends = ["numbers"]
    command = ["sh", "-c", "grep -cE '[02468]$' ${scratch}/numbers.txt | tee ${scratch}/evens.txt"]
  }

  // Waits on both branches. If either fails, `report` is skipped.
  task "report" {
    depends = ["total", "evens"]
    command = ["sh", "-c", <<-EOT
      echo "report for ${date}"
      echo "  total: $(cat ${scratch}/total.txt)"
      echo "  evens: $(cat ${scratch}/evens.txt)"
    EOT
    ]
  }
}
