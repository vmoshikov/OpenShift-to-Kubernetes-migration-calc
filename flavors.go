package main

// DefaultFlavors — каталог "бариков" целевого Mk8s. Для демо — условные цифры,
// в реальном расчёте сюда подставляется прайс конкретного облака
// (Yandex Cloud / VK Cloud / Cloud.ru / собственный bare-metal).
func DefaultFlavors() []NodeFlavor {
	return []NodeFlavor{
		{Name: "s-4x16", CPUMilli: 4000, MemMiB: 16 * 1024, RelativeCost: 9.5},
		{Name: "s-8x32", CPUMilli: 8000, MemMiB: 32 * 1024, RelativeCost: 18.0},
		{Name: "s-16x64", CPUMilli: 16000, MemMiB: 64 * 1024, RelativeCost: 34.0},
		{Name: "s-32x128", CPUMilli: 32000, MemMiB: 128 * 1024, RelativeCost: 65.0},
	}
}
