package main

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "maps"
    "os"
    "path/filepath"
	"strconv"

    cdi "tags.cncf.io/container-device-interface/specs-go"
    resourcev1 "k8s.io/api/resource/v1"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/runtime/serializer"
    "k8s.io/apimachinery/pkg/types"
    utilruntime "k8s.io/apimachinery/pkg/util/runtime"
    coreclientset "k8s.io/client-go/kubernetes"
    "k8s.io/dynamic-resource-allocation/kubeletplugin"
    "k8s.io/dynamic-resource-allocation/resourceslice"
    "k8s.io/klog/v2"

    // rtv1alpha1 "sigs.k8s.io/dra-example-driver/api/example.com/resource/rt/v1alpha1"
    "sigs.k8s.io/dra-example-driver/pkg/consts"
)

type driver struct {
    client      coreclientset.Interface
    helper      *kubeletplugin.Helper
    state       *DeviceState
    healthcheck *healthcheck
    cancelCtx   func(error)
    scheme      *runtime.Scheme
    decoder     runtime.Decoder
}

func NewDriver(ctx context.Context, config *Config, scheme *runtime.Scheme) (*driver, error) {
    codecFactory := serializer.NewCodecFactory(scheme)
    driver := &driver{
        client:    config.coreclient,
        cancelCtx: config.cancelMainCtx,
        scheme:    scheme,
        decoder:   codecFactory.UniversalDeserializer(),
    }
    state, err := NewDeviceState(config)
    if err != nil {
        return nil, err
    }
    driver.state = state

    helper, err := kubeletplugin.Start(
        ctx, driver, kubeletplugin.KubeClient(config.coreclient),
        kubeletplugin.NodeName(config.flags.nodeName),
        kubeletplugin.DriverName(consts.DriverName),
        kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
        kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
    )
    if err != nil {
        return nil, err
    }
    driver.helper = helper

    devices := make([]resourcev1.Device, 0, len(state.allocatable))
    for device := range maps.Values(state.allocatable) {
        devices = append(devices, device)
    }
    resources := resourceslice.DriverResources{
        Pools: map[string]resourceslice.Pool{
            config.flags.nodeName: {
                Slices: []resourceslice.Slice{{Devices: devices}},
            },
        },
    }
    driver.healthcheck, err = startHealthcheck(ctx, config)
    if err != nil {
        return nil, fmt.Errorf("start healthcheck: %w", err)
    }
    if err := helper.PublishResources(ctx, resources); err != nil {
        return nil, err
    }
    return driver, nil
}

// 최신 DRA API 시그니처 사용
func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourcev1.ResourceClaim) (result map[types.UID]kubeletplugin.PrepareResult, err error) {
    klog.Infof("PrepareResourceClaims is called: number of claims: %d", len(claims))
    result = make(map[types.UID]kubeletplugin.PrepareResult)
    for _, claim := range claims {
        result[claim.UID] = d.prepareResourceClaim(ctx, claim)
    }
    return result, nil
}

func (d *driver) prepareResourceClaim(ctx context.Context, claim *resourcev1.ResourceClaim) kubeletplugin.PrepareResult {
    // 실시간 파라미터가 있는지 확인
    if claim.Spec.Devices.Requests == nil || len(claim.Spec.Devices.Requests) == 0 {
        klog.Infof("Claim '%v' has no device requests, using default device state logic.", claim.UID)
        return d.prepareWithDeviceState(claim)
    }

    // 첫 번째 request에서 파라미터 확인 (실제로는 모든 request를 처리해야 함)
    // request := claim.Spec.Devices.Requests[0]
    
    // DeviceClass로부터 실시간 파라미터를 추출하는 로직 구현
    // 여기서는 간소화된 버전으로 환경변수나 annotation을 통해 실시간 파라미터를 전달받는다고 가정
    
    // 실시간 스케줄링 CDI 파일 생성
    if runtimeVal, periodVal := d.extractRealtimeParams(claim); runtimeVal > 0 && periodVal > 0 {
        return d.createRealtimeCDI(claim, runtimeVal, periodVal)
    }

    klog.Infof("Claim '%v' has no realtime parameters, using default logic.", claim.UID)
    return d.prepareWithDeviceState(claim)
}

// 실시간 파라미터 추출 (annotation 또는 다른 방법 사용)
func (d *driver) extractRealtimeParams(claim *resourcev1.ResourceClaim) (runtime, period uint64) {
    annotations := claim.GetAnnotations()
    if annotations == nil {
        return 0, 0
    }
    
    // annotation에서 실시간 파라미터 추출
    if runtimeStr, exists := annotations["rt.resource.example.com/runtime"]; exists {
        if parsedRuntime := parseUint64(runtimeStr); parsedRuntime > 0 {
            runtime = parsedRuntime
        }
    }
    
    if periodStr, exists := annotations["rt.resource.example.com/period"]; exists {
        if parsedPeriod := parseUint64(periodStr); parsedPeriod > 0 {
            period = parsedPeriod
        }
    }
    
    return runtime, period
}

// uint64 파싱 헬퍼 함수
func parseUint64(s string) uint64 {
    if val, err := strconv.ParseUint(s, 10, 64); err == nil {
        return val
    }
    return 0
}

// 실시간 CDI 파일 생성
func (d *driver) createRealtimeCDI(claim *resourcev1.ResourceClaim, runtimeVal, periodVal uint64) kubeletplugin.PrepareResult {
    containerEdits := cdi.ContainerEdits{
        Env: []string{
            fmt.Sprintf("RT_RUNTIME=%d", runtimeVal),
            fmt.Sprintf("RT_PERIOD=%d", periodVal),
            "RT_SCHEDULER=SCHED_DEADLINE",
        },
    }

    cdiDeviceName := fmt.Sprintf("rt.resource.example.com/realtime-%s", claim.UID)
    cdiSpec := &cdi.Spec{
        Version: "0.8.0",
        Kind:    "rt.resource.example.com/realtime",
        Devices: []cdi.Device{
            {
                Name:           "realtime-profile",
                ContainerEdits: containerEdits,
            },
        },
    }

    cdiRootDir := "/etc/cdi"
    if err := os.MkdirAll(cdiRootDir, 0755); err != nil {
        return kubeletplugin.PrepareResult{
            Err: fmt.Errorf("failed to create CDI directory: %w", err),
        }
    }

    specFileName := filepath.Join(cdiRootDir, fmt.Sprintf("%s.json", cdiDeviceName))
    specFile, err := os.Create(specFileName)
    if err != nil {
        return kubeletplugin.PrepareResult{
            Err: fmt.Errorf("failed to create CDI spec file: %w", err),
        }
    }
    defer specFile.Close()

    if err := json.NewEncoder(specFile).Encode(cdiSpec); err != nil {
        return kubeletplugin.PrepareResult{
            Err: fmt.Errorf("failed to write CDI spec file: %w", err),
        }
    }

    klog.Infof("Generated CDI device '%s' for claim '%v' with runtime=%d, period=%d", 
               cdiDeviceName, claim.UID, runtimeVal, periodVal)
               
    return kubeletplugin.PrepareResult{
        Devices: []kubeletplugin.Device{
            {CDIDeviceIDs: []string{cdiDeviceName}},
        },
    }
}

func (d *driver) prepareWithDeviceState(claim *resourcev1.ResourceClaim) kubeletplugin.PrepareResult {
    preparedPBs, err := d.state.Prepare(claim)
    if err != nil {
        return kubeletplugin.PrepareResult{
            Err: fmt.Errorf("error preparing devices for claim %v: %w", claim.UID, err),
        }
    }
    var prepared []kubeletplugin.Device
    for _, preparedPB := range preparedPBs {
        prepared = append(prepared, kubeletplugin.Device{
            Requests:     preparedPB.GetRequestNames(),
            PoolName:     preparedPB.GetPoolName(),
            DeviceName:   preparedPB.GetDeviceName(),
            CDIDeviceIDs: preparedPB.GetCDIDeviceIDs(),
        })
    }
    klog.Infof("Returning newly prepared devices for claim '%v': %v", claim.UID, prepared)
    return kubeletplugin.PrepareResult{Devices: prepared}
}

func (d *driver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
    klog.Infof("UnprepareResourceClaims is called: number of claims: %d", len(claims))
    result := make(map[types.UID]error)
    for _, claim := range claims {
        result[claim.UID] = d.unprepareResourceClaim(ctx, claim)
    }
    return result, nil
}

func (d *driver) unprepareResourceClaim(_ context.Context, claim kubeletplugin.NamespacedObject) error {
    if err := d.state.Unprepare(string(claim.UID)); err != nil {
        return fmt.Errorf("error unpreparing devices for claim %v: %w", claim.UID, err)
    }
    return nil
}

func (d *driver) Shutdown(logger klog.Logger) error {
    if d.healthcheck != nil {
        d.healthcheck.Stop(logger)
    }
    d.helper.Stop()
    return nil
}

func (d *driver) HandleError(ctx context.Context, err error, msg string) {
    utilruntime.HandleErrorWithContext(ctx, err, msg)
    if !errors.Is(err, kubeletplugin.ErrRecoverable) && d.cancelCtx != nil {
        d.cancelCtx(fmt.Errorf("fatal background error: %w", err))
    }
}
