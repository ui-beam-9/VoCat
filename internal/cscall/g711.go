package cscall

// G.711 μ-law 编解码（ITU-T G.711）。
//
// 算法与 internal/vowifi/ims/rtp_media.go 中的实现保持一致，用于 16-bit
// 线性 PCM ↔ 8-bit μ-law 的转换（CS 域语音桥使用 PCMU 与软电话对接）。

// LinearToUlaw 将 16-bit 线性 PCM 样本转换为 8-bit μ-law。
func LinearToUlaw(sample int16) byte {
	value := int(sample)
	sign := byte(0)
	if value < 0 {
		sign, value = 0x80, -value
		if value > 32767 {
			value = 32767
		}
	}
	value += 132
	if value > 32635 {
		value = 32635
	}
	exponent := 7
	for mask := 0x4000; exponent > 0 && value&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := (value >> (exponent + 3)) & 0x0f
	return ^(sign | byte(exponent<<4) | byte(mantissa))
}

// UlawToLinear 将 8-bit μ-law 转换为 16-bit 线性 PCM 样本。
func UlawToLinear(value byte) int16 {
	value = ^value
	magnitude := ((int(value)&0x0f)<<3 + 132) << ((value & 0x70) >> 4)
	magnitude -= 132
	if value&0x80 != 0 {
		return int16(-magnitude)
	}
	return int16(magnitude)
}

// EncodePCMToUlaw 批量把 S16_LE 的 PCM 编码为 μ-law。
func EncodePCMToUlaw(pcm []byte) []byte {
	numSamples := len(pcm) / 2
	out := make([]byte, numSamples)
	for i := 0; i < numSamples; i++ {
		sample := int16(pcm[i*2]) | int16(pcm[i*2+1])<<8
		out[i] = LinearToUlaw(sample)
	}
	return out
}

// DecodeUlawToPCM 批量把 μ-law 解码为 S16_LE 的 PCM。
func DecodeUlawToPCM(ulaw []byte) []byte {
	out := make([]byte, len(ulaw)*2)
	for i, u := range ulaw {
		sample := UlawToLinear(u)
		out[i*2] = byte(sample)
		out[i*2+1] = byte(sample >> 8)
	}
	return out
}
