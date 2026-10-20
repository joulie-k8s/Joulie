// peercopy: device to device copy timing and a memory-bound load, for the
// receiver gate and power measurements of hwmeasure.sh. One source builds
// with nvcc (CUDA) and with hipcc (HIP); probe.sh builds it inside the pod.
//
//   peercopy list
//   peercopy copy --src BUS --dst BUS [--bytes N] [--iters N]
//                 [--issuer src|dst] [--cold-s S] [--gap-ms MS]
//                 [--receiver-load none|stream] [--peer-access on|off]
//   peercopy stream --device BUS --seconds S [--bytes N]
//
// Devices are PCI bus ids (0000:3b:00.0), never runtime indices, because
// the runtimes and the management tools enumerate in different orders.
//
// copy allocates BYTES on both GPUs, waits --cold-s seconds so an idle
// receiver drops back to its idle clocks, then times --iters copies from
// src into dst, each one alone on a stream of the issuing GPU's context,
// and prints one line per copy, "copy iter=I bytes=N ms=T gbps=R", then
// "summary median_gbps=R min_gbps=R max_gbps=R peer_access=0|1". With
// --receiver-load stream a second thread keeps a memory-bound kernel
// running on dst from before the first copy to after the last.
//
// stream keeps a memory-bound kernel running on one GPU for --seconds and
// prints "stream device=BUS seconds=S passes=K gbps=R".

#if defined(__HIPCC__)
#include <hip/hip_runtime.h>
#define API(name) hip##name
typedef hipDeviceProp_t DeviceProp;
#else
#include <cuda_runtime.h>
#define API(name) cuda##name
typedef cudaDeviceProp DeviceProp;
#endif

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

#define CHECK(call)                                                          \
  do {                                                                       \
    API(Error_t) e_ = (call);                                                \
    if (e_ != API(Success)) {                                                \
      fprintf(stderr, "peercopy: %s failed: %s\n", #call,                   \
              API(GetErrorString)(e_));                                      \
      exit(2);                                                               \
    }                                                                        \
  } while (0)

// touch reads and writes every element of buf reps times: memory bound.
__global__ void touch(float4 *buf, size_t n, int reps) {
  size_t i = blockIdx.x * (size_t)blockDim.x + threadIdx.x;
  size_t stride = (size_t)gridDim.x * blockDim.x;
  for (int r = 0; r < reps; r++) {
    for (size_t j = i; j < n; j += stride) {
      float4 v = buf[j];
      v.x += 1.0f;
      buf[j] = v;
    }
  }
}

static void usage() {
  fprintf(stderr,
          "usage: peercopy list\n"
          "       peercopy copy --src BUS --dst BUS [--bytes N] [--iters N] "
          "[--issuer src|dst] [--cold-s S] [--gap-ms MS] "
          "[--receiver-load none|stream] [--peer-access on|off]\n"
          "       peercopy stream --device BUS --seconds S [--bytes N]\n");
  exit(2);
}

static int device_of(const std::string &bus) {
  int dev = -1;
  CHECK(API(DeviceGetByPCIBusId)(&dev, bus.c_str()));
  return dev;
}

static double now_s() {
  return std::chrono::duration<double>(
             std::chrono::steady_clock::now().time_since_epoch())
      .count();
}

// run_stream keeps touch running on dev until stop is set or seconds pass,
// and returns the passes made and the bytes moved.
static void run_stream(int dev, size_t bytes, double seconds,
                       std::atomic<bool> *stop, long *passes, double *moved) {
  CHECK(API(SetDevice)(dev));
  size_t n = bytes / sizeof(float4);
  float4 *buf = nullptr;
  CHECK(API(Malloc)(reinterpret_cast<void **>(&buf), n * sizeof(float4)));
  CHECK(API(Memset)(buf, 0, n * sizeof(float4)));
  API(Stream_t) s;
  CHECK(API(StreamCreate)(&s));
  DeviceProp prop;
  CHECK(API(GetDeviceProperties)(&prop, dev));
  int blocks = prop.multiProcessorCount * 8;
  const int reps = 4;
  double start = now_s();
  long k = 0;
  while ((stop == nullptr || !stop->load()) && now_s() - start < seconds) {
    touch<<<blocks, 256, 0, s>>>(buf, n, reps);
    CHECK(API(GetLastError)());
    CHECK(API(StreamSynchronize)(s));
    k++;
  }
  CHECK(API(StreamDestroy)(s));
  CHECK(API(Free)(buf));
  if (passes) *passes = k;
  if (moved) *moved = 2.0 * reps * (double)k * (double)(n * sizeof(float4));
}

static int cmd_list() {
  int count = 0;
  CHECK(API(GetDeviceCount)(&count));
  for (int d = 0; d < count; d++) {
    char bus[32];
    DeviceProp prop;
    CHECK(API(DeviceGetPCIBusId)(bus, sizeof bus, d));
    CHECK(API(GetDeviceProperties)(&prop, d));
    printf("device index=%d bus=%s name=\"%s\"\n", d, bus, prop.name);
  }
  return 0;
}

static int cmd_stream(const std::string &bus, double seconds, size_t bytes) {
  long passes = 0;
  double moved = 0;
  double t0 = now_s();
  run_stream(device_of(bus), bytes, seconds, nullptr, &passes, &moved);
  double t = now_s() - t0;
  printf("stream device=%s seconds=%.1f passes=%ld gbps=%.2f\n", bus.c_str(),
         t, passes, moved / t / 1e9);
  return 0;
}

static int cmd_copy(const std::string &src_bus, const std::string &dst_bus,
                    size_t bytes, int iters, bool issue_on_dst, double cold_s,
                    double gap_ms, bool receiver_stream, bool peer_access) {
  int src = device_of(src_bus), dst = device_of(dst_bus);
  int issuer = issue_on_dst ? dst : src, other = issue_on_dst ? src : dst;
  int can = 0;
  CHECK(API(DeviceCanAccessPeer)(&can, issuer, other));
  bool enabled = false;
  if (can && peer_access) {
    CHECK(API(SetDevice)(issuer));
    API(Error_t) e = API(DeviceEnablePeerAccess)(other, 0);
    if (e != API(Success) && e != API(ErrorPeerAccessAlreadyEnabled)) {
      fprintf(stderr, "peercopy: enabling peer access failed: %s\n",
              API(GetErrorString)(e));
      exit(2);
    }
    (void)API(GetLastError)();
    enabled = true;
  }
  void *sbuf = nullptr, *dbuf = nullptr;
  CHECK(API(SetDevice)(src));
  CHECK(API(Malloc)(&sbuf, bytes));
  CHECK(API(Memset)(sbuf, 1, bytes));
  CHECK(API(DeviceSynchronize)());
  CHECK(API(SetDevice)(dst));
  CHECK(API(Malloc)(&dbuf, bytes));
  CHECK(API(DeviceSynchronize)());

  std::atomic<bool> stop(false);
  std::thread load;
  if (receiver_stream) {
    load = std::thread(run_stream, dst, (size_t)256 << 20, 1e9, &stop,
                       nullptr, nullptr);
    std::this_thread::sleep_for(std::chrono::seconds(1));
  } else if (cold_s > 0) {
    std::this_thread::sleep_for(std::chrono::duration<double>(cold_s));
  }

  CHECK(API(SetDevice)(issuer));
  API(Stream_t) s;
  API(Event_t) a, b;
  CHECK(API(StreamCreate)(&s));
  CHECK(API(EventCreate)(&a));
  CHECK(API(EventCreate)(&b));
  std::vector<double> rates;
  for (int i = 0; i < iters; i++) {
    CHECK(API(EventRecord)(a, s));
    CHECK(API(MemcpyPeerAsync)(dbuf, dst, sbuf, src, bytes, s));
    CHECK(API(EventRecord)(b, s));
    CHECK(API(EventSynchronize)(b));
    float ms = 0;
    CHECK(API(EventElapsedTime)(&ms, a, b));
    double gbps = (double)bytes / (ms / 1e3) / 1e9;
    rates.push_back(gbps);
    printf("copy iter=%d bytes=%zu ms=%.4f gbps=%.3f\n", i, bytes, ms, gbps);
    fflush(stdout);
    if (gap_ms > 0)
      std::this_thread::sleep_for(std::chrono::duration<double, std::milli>(gap_ms));
  }
  if (receiver_stream) {
    stop = true;
    load.join();
  }
  CHECK(API(SetDevice)(issuer));
  CHECK(API(EventDestroy)(a));
  CHECK(API(EventDestroy)(b));
  CHECK(API(StreamDestroy)(s));
  CHECK(API(SetDevice)(src));
  CHECK(API(Free)(sbuf));
  CHECK(API(SetDevice)(dst));
  CHECK(API(Free)(dbuf));
  std::vector<double> sorted = rates;
  std::sort(sorted.begin(), sorted.end());
  double med = 0;
  if (!sorted.empty()) {
    size_t m = sorted.size();
    med = (m % 2) ? sorted[m / 2] : (sorted[m / 2 - 1] + sorted[m / 2]) / 2;
  }
  printf("summary median_gbps=%.3f min_gbps=%.3f max_gbps=%.3f peer_access=%d\n",
         med, sorted.empty() ? 0 : sorted.front(),
         sorted.empty() ? 0 : sorted.back(), enabled ? 1 : 0);
  return 0;
}

int main(int argc, char **argv) {
  if (argc < 2) usage();
  std::string cmd = argv[1];
  std::string src, dst, device, issuer = "src", load = "none", peer = "on";
  size_t bytes = 0;
  int iters = 12;
  double cold_s = 10, gap_ms = 0, seconds = 0;
  for (int i = 2; i < argc; i++) {
    std::string k = argv[i];
    if (i + 1 >= argc) usage();
    std::string v = argv[++i];
    if (k == "--src") src = v;
    else if (k == "--dst") dst = v;
    else if (k == "--device") device = v;
    else if (k == "--bytes") bytes = strtoull(v.c_str(), nullptr, 10);
    else if (k == "--iters") iters = atoi(v.c_str());
    else if (k == "--issuer") issuer = v;
    else if (k == "--cold-s") cold_s = atof(v.c_str());
    else if (k == "--gap-ms") gap_ms = atof(v.c_str());
    else if (k == "--receiver-load") load = v;
    else if (k == "--peer-access") peer = v;
    else if (k == "--seconds") seconds = atof(v.c_str());
    else usage();
  }
  if (cmd == "list") return cmd_list();
  if (cmd == "stream") {
    if (device.empty() || seconds <= 0) usage();
    return cmd_stream(device, seconds, bytes ? bytes : (size_t)1 << 30);
  }
  if (cmd == "copy") {
    if (src.empty() || dst.empty() || iters < 1) usage();
    if (issuer != "src" && issuer != "dst") usage();
    if (load != "none" && load != "stream") usage();
    if (peer != "on" && peer != "off") usage();
    return cmd_copy(src, dst, bytes ? bytes : (size_t)64 << 20, iters,
                    issuer == "dst", cold_s, gap_ms, load == "stream",
                    peer == "on");
  }
  usage();
}
