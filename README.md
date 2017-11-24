# kong-ingress-controller
A simple Kubernetes ingress controller for the Kong API gateway

## Usage

```
  -alsologtostderr
        log to standard error as well as files
  -externalapi
        connect to the API from outside the kubernetes cluster
  -kongaddress string
        address of the kong API server (default "http://kong-admin:8001")
  -kubeconfig string
        (optional) absolute path to the kubeconfig file (default "/Users/dale/.kube/config")
  -log_backtrace_at value
        when logging hits line file:N, emit a stack trace
  -log_dir string
        If non-empty, write log files in this directory
  -logtostderr
        log to standard error instead of files
  -stderrthreshold value
        logs at or above this threshold go to stderr
  -v value
        log level for V logs
  -vmodule value
        comma-separated list of pattern=N settings for file-filtered logging
```

## Restrictions
The controller currently only handles a very restricted subset of Ingress resources. 
It supports non-TLS ingresses with a single rule and a single root path.
