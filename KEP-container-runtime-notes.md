# Proposal

**status:** Draft

Expose KeptnTask logs via OpenTelemetry.

## Technical Details

Auto/Manual-instrumentation can rely on the well-known env var `OTEL_COLLECTOR`.
If user doesn't support OTel, KLT shall provide this functionality out-of-the-box.
For this, the KeptnTask controller will create a Job with multiple containers: 
one with the task and one with a small control-plane-like process.
The control-plane process will get via env var:
 - the W3C trace-id to start a trace;
 - the OTel collector URL to expose the data.

This process shall also collect logs from the other container and expose them to the collector.

this is also runtime-agnostic. 

### Notes

`cat` exists when the other process finishes writing on stdout. This makes orchestrating the processes easy.
There might be problems in "locking" the process from writing in the stdout while reading it from the other process. From initial limited testing, this might be only visible to the I/O with the terminal. Further stress-test are necessary.

## Known limitation / Alternative solutions

This might rise some security concerns given the requirement of running the control-plane-like process as a sudo user.
Furthermore, it looks like we get back the logs **only** at process exit, which could result in a memory problem.
We might require a log file as a parameter (optional) to decide where to read the log from.

## PoC via manifest

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: keptn-task
spec:
  shareProcessNamespace: true # required to be able to read container task
  containers:
  - name: process
    image: ghcr.keptn.sh/keptn/functions-runtime:v0.5.0
    env:
    - name: SCRIPT
      value: https://raw.githubusercontent.com/thisthat/lifecycle-controller/test/test2.ts
  - name: keptn-runtime
    image: busybox:1.28
    command:
    - sh
    - -c
    - |
      sleep 1
      ps -a
      pid=`ps -ef | grep deno | head -n 1 | awk '{print $1}'`
      echo ${pid}
      cat /proc/${pid}/fd/1
      echo "<done>"
      sleep 5000
    securityContext:
      allowPrivilegeEscalation: false
      runAsUser: 0
      capabilities:
        add:
        - SYS_PTRACE
    stdin: true
    tty: true
```
