// SPDX-License-Identifier: Apache-2.0
//
// A GPU load generator that holds a *target* utilisation rather than pinning
// the card, for producing dashboard captures that look like a real tenant.
//
// Why this exists: the usual stand-in, `nbody -benchmark`, queues kernels
// back-to-back and reports 100% at every body count, so it cannot draw
// anything but a flat line. Gating it externally does not work either --
// SIGSTOP leaves the already-queued work running and the card stays busy.
// The only way to sit at 40% is to leave the card genuinely idle 60% of the
// time, at a granularity finer than NVML's ~1s sampling window.
//
//   gpu-load <mem_mb> <duty_low> <duty_high> <seed>
//
// Duty follows a bounded random walk between duty_low and duty_high so the
// series drifts like a real workload instead of stepping between two values.
//
// Build: nvcc -O2 -o gpu-load gpu-load.cu

#include <cstdio>
#include <cstdlib>
#include <ctime>
#include <unistd.h>
#include <cuda_runtime.h>

__global__ void burn(float *buf, size_t n, int iters) {
    size_t i = blockIdx.x * (size_t)blockDim.x + threadIdx.x;
    if (i >= n) return;
    float x = buf[i];
    for (int k = 0; k < iters; ++k) x = fmaf(x, 1.0000001f, 0.0000001f);
    buf[i] = x;
}

static double now_ms() {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec * 1000.0 + ts.tv_nsec / 1e6;
}

int main(int argc, char **argv) {
    int mem_mb    = argc > 1 ? atoi(argv[1]) : 512;
    int duty_low  = argc > 2 ? atoi(argv[2]) : 20;
    int duty_high = argc > 3 ? atoi(argv[3]) : 80;
    unsigned seed = argc > 4 ? (unsigned)atoi(argv[4]) : 1;

    size_t n = (size_t)mem_mb * 1024 * 1024 / sizeof(float);
    float *buf = nullptr;
    if (cudaMalloc(&buf, n * sizeof(float)) != cudaSuccess) {
        fprintf(stderr, "cudaMalloc of %d MiB failed\n", mem_mb);
        return 1;
    }
    cudaMemset(buf, 0, n * sizeof(float));

    int threads = 256;
    int blocks  = (int)((n + threads - 1) / threads);
    if (blocks > 65535) blocks = 65535;

    // Calibrate: find an iteration count that makes one launch last ~5ms. It
    // has to be well under the duty window below, which in turn has to be well
    // under NVML's ~1s sampling window -- otherwise a sample lands entirely
    // inside one busy or idle phase and reads 100 or 0, and the series is a
    // square wave rather than a utilisation.
    int iters = 64;
    for (int attempt = 0; attempt < 24; ++attempt) {
        double t0 = now_ms();
        burn<<<blocks, threads>>>(buf, n, iters);
        cudaDeviceSynchronize();
        double el = now_ms() - t0;
        if (el > 4.0 && el < 7.0) break;
        double scale = el > 0.2 ? 5.0 / el : 4.0;
        if (scale > 4.0) scale = 4.0;
        if (scale < 0.25) scale = 0.25;
        int next = (int)(iters * scale);
        iters = next < 1 ? 1 : next;
    }
    double t0 = now_ms();
    burn<<<blocks, threads>>>(buf, n, iters);
    cudaDeviceSynchronize();
    double launch_ms = now_ms() - t0;
    if (launch_ms < 1.0) launch_ms = 1.0;
    printf("gpu-load: %d MiB, duty %d-%d%%, %.1f ms/launch\n",
           mem_mb, duty_low, duty_high, launch_ms);
    fflush(stdout);

    srand(seed);
    double duty = (duty_low + duty_high) / 2.0;
    const double window_ms = 120.0;   // many of these per NVML sample

    while (true) {
        // Hold a duty for a few seconds at a time. Re-rolling it every window
        // would just look like noise; a real tenant sits at a level and then
        // moves. Random walk reflected at the bounds, with an occasional jump
        // so the curve has steps in it and not only drift.
        duty += ((rand() % 200) - 100) / 100.0 * 6.0;
        if (rand() % 6 == 0) duty += ((rand() % 200) - 100) / 100.0 * 18.0;
        if (duty < duty_low)  duty = duty_low  + (duty_low - duty);
        if (duty > duty_high) duty = duty_high - (duty - duty_high);
        if (duty < 1) duty = 1;
        if (duty > 100) duty = 100;

        double phase_ms = 3000.0 + (rand() % 5000);
        int windows = (int)(phase_ms / window_ms);

        for (int w = 0; w < windows; ++w) {
            double busy_ms = window_ms * duty / 100.0;
            int launches = (int)(busy_ms / launch_ms + 0.5);
            if (launches < 1) launches = 1;

            double b0 = now_ms();
            for (int i = 0; i < launches; ++i) burn<<<blocks, threads>>>(buf, n, iters);
            cudaDeviceSynchronize();
            double spent = now_ms() - b0;

            double idle_ms = window_ms - spent;
            if (idle_ms > 0) usleep((useconds_t)(idle_ms * 1000));
        }
    }
    return 0;
}
