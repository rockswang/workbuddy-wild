package winutil

import (
	"math"
	"testing"
)

// recons 由 AnchorPanel 返回的物理位置 + DIP 尺寸，重建出物理尺寸，便于与屏幕做越界断言。
func physicalW(cwDip, chDip int, sc float64) (int, int) {
	return int(math.Round(float64(cwDip) * sc)), int(math.Round(float64(chDip) * sc))
}

// TestAnchorPanel 锚点计算：返回的物理位置必须让重建后的物理窗口完整落在屏幕内（不越出右下角）。
// 关键不变量：WindowSetPosition 收物理坐标、WindowSetSize 收 DIP（内部再放大 sc 回物理），
// 两者必须一致到同一个物理矩形，否则贴右下的面板会在缩放显示屏上越界。
func TestAnchorPanel(t *testing.T) {
	type args struct {
		waX, waY, waW, waH  int
		tbX, tbY, tbW, tbH  int
		sc                  float64
		scrW, scrH          int
		pwDip, phDip, gapDip int
	}
	tests := []struct {
		name                          string
		args                          args
		wantX, wantY, wantWDip, wantHDip int
	}{
		{
			// 125% 缩放（回归）：物理 1920x1080、底部任务栏 40。
			// 修复前：窗口物理尺寸被 SetSize 放大 1.25 倍（270→338、500→625），
			// 位置却按 270x500 算 → 右缘 1976>1920、底缘 1153>1080，整块越出右下角。
			name: "底部任务栏、125% 缩放（回归：物理尺寸重建后必须完整在屏内）",
			args: args{0, 0, 1920, 1040, 0, 1040, 1920, 40, 1.25, 1920, 1080, 270, 500, 12},
			// 物理位置：x=1920-338-15=1567, y=1040-625-15=400；DIP 尺寸 270x500
			wantX: 1567, wantY: 400, wantWDip: 270, wantHDip: 500,
		},
		{
			name: "底部任务栏、100% 缩放",
			args: args{0, 0, 1920, 1040, 0, 1040, 1920, 40, 1, 1920, 1080, 270, 500, 12},
			wantX: 1638, wantY: 528, wantWDip: 270, wantHDip: 500,
		},
		{
			name: "无任务栏、回退工作区右下角",
			args: args{0, 0, 1920, 1080, 0, 0, 0, 0, 1, 1920, 1080, 270, 500, 12},
			wantX: 1638, wantY: 568, wantWDip: 270, wantHDip: 500,
		},
		{
			name: "右侧垂直任务栏",
			args: args{0, 0, 1860, 1080, 1860, 0, 60, 1080, 1, 1920, 1080, 270, 500, 12},
			wantX: 1578, wantY: 568, wantWDip: 270, wantHDip: 500,
		},
		{
			name: "左侧垂直任务栏",
			args: args{60, 0, 1860, 1080, 0, 0, 60, 1080, 1, 1920, 1080, 270, 500, 12},
			wantX: 72, wantY: 568, wantWDip: 270, wantHDip: 500,
		},
		{
			name: "顶部任务栏",
			args: args{0, 40, 1920, 1040, 0, 0, 1920, 40, 1, 1920, 1080, 270, 500, 12},
			wantX: 1638, wantY: 52, wantWDip: 270, wantHDip: 500,
		},
		{
			// 屏幕过小：面板 DIP 尺寸再放大 sc 后超过屏幕，必须钳制到屏幕内（退出按钮可达）。
			name: "屏幕过小且高缩放：物理重建后必须落地且不越界",
			args: args{0, 0, 900, 700, 0, 0, 0, 0, 1.25, 900, 700, 270, 900, 12},
			// 物理 ph 被钳到 700、pw=338；位置 x=547,y=0；DIP = 270x560
			wantX: 547, wantY: 0, wantWDip: 270, wantHDip: 560,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x, y, cw, ch := AnchorPanel(
				tt.args.waX, tt.args.waY, tt.args.waW, tt.args.waH,
				tt.args.tbX, tt.args.tbY, tt.args.tbW, tt.args.tbH,
				tt.args.sc, tt.args.scrW, tt.args.scrH,
				tt.args.pwDip, tt.args.phDip, tt.args.gapDip,
			)
			if x != tt.wantX || y != tt.wantY || cw != tt.wantWDip || ch != tt.wantHDip {
				t.Errorf("AnchorPanel() = phys(%d,%d) dip(%dx%d), want phys(%d,%d) dip(%dx%d)",
					x, y, cw, ch, tt.wantX, tt.wantY, tt.wantWDip, tt.wantHDip)
			}
			// 不变量：SetSize 用 DIP 放大 sc 后，物理矩形 (x,y,w,h) 必须完整在屏幕内。
			pw, ph := physicalW(cw, ch, tt.args.sc)
			if pw != 0 && ph != 0 {
				if x < 0 || y < 0 || x+pw > tt.args.scrW || y+ph > tt.args.scrH {
					t.Errorf("physical window (%d,%d,%d,%d) escapes screen (0,0,%d,%d)", x, y, pw, ph, tt.args.scrW, tt.args.scrH)
				}
			}
		})
	}
}