# gracenet
Wrapper around net.Listener with network activity monitoring

## Example usage
In this example application exits after 5 minutes without any network activity

```golang

...

func main() {
    ln, err := net.Listen("tcp", "localhost:8080")
    defer ln.Close()
    if err != nil {
        return errors.Wrap(err, "Failed to bind address")
    }
    agln := gracenet.NewGraceListener(ln, 5 * time.Minute) // setting grace period
    httpError := make(chan error, 1)
    go func() {
        mux := http.NewServeMux()
        err = http.Serve(agln, mux)
        httpError <- err
    }()
    sigs := make(chan os.Signal, 1)
    signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

    select {
    case <-agln.Expire():
        log.Info("Nobody here for so long!")
    case sig := <-sigs:
        log.WithField("signal", sig).Info("Got syscall")
    case err := <-httpError:
        return errors.Wrap(err, "Got http error")
    }
    log.Info("Shooting down... at last!")
    return nil
}
```
