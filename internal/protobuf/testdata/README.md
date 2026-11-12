# Protobuf request bodies sent by the real kubectl

- `kubectl-v1.36.6/NAME.bin`: the POST body kubectl v1.36.6 (linux/amd64) sent for the command listed in `hack/m1-capture.sh`. Captured 2026-11-10 with `sw server --trace`.
- `kubectl-v1.36.0-windows/NAME.bin`: the same commands from kubectl.exe v1.36.0. All ten bodies are byte-for-byte equal to the v1.36.6 ones (the decoder test checks this).
- `kubectl-v1.36.6/NAME.json`: the same object as Kubernetes itself decodes it (apimachinery v0.36.6 protobuf serializer, 2026-11-10), before defaulting. The decoder must produce an object equal to what `apis.Decode` makes of this JSON.

`debug-copy-pod` is the only kubectl command found that sends a Pod as Protobuf: `kubectl debug POD --copy-to=NAME` creates the copy through the typed client. `create-service-nodeport` is here as a body the server must decode and then refuse (422, unsupported type).

Do not edit these files. To add a case or a client version, extend `hack/m1-capture.sh` and run it again. The `.json` for a new body has to come from Kubernetes itself. The small program that wrote these (a separate Go module that imports `k8s.io/api` and `k8s.io/apimachinery`; `sw` never did) was removed from the repository on 2026-11-11 and is in git history as `hack/oracle` at commit `483e994` (`git show 483e994:hack/oracle/main.go`).
