import SwiftUI

struct InfiniteStreamLogo: View {
    var body: some View {
        Canvas { ctx, size in
            let scale = min(size.width, size.height) / 48
            ctx.transform = CGAffineTransform(scaleX: scale, y: scale)

            // Background
            ctx.fill(
                Path(CGRect(x: 0, y: 0, width: 48, height: 48)),
                with: .color(Color(red: 9/255, green: 25/255, blue: 41/255))
            )

            let right = rightLobe
            let left  = leftLobe
            let roundCap = StrokeStyle(lineWidth: 0, lineCap: .round, lineJoin: .round)

            func stroke(_ path: Path, color: Color, width: CGFloat) {
                ctx.stroke(path, with: .color(color),
                           style: StrokeStyle(lineWidth: width, lineCap: .round, lineJoin: .round))
            }

            // Layered strokes — drawn back-to-front (shadow → highlight)
            let shadow    = Color(red: 1/255,   green: 46/255,  blue: 71/255)
            let water     = Color(red: 0,        green: 119/255, blue: 182/255)
            let midWater  = Color(red: 0,        green: 180/255, blue: 216/255)
            let highlight = Color(red: 173/255,  green: 232/255, blue: 244/255)

            stroke(right, color: shadow,    width: 11)
            stroke(left,  color: shadow,    width: 11)
            stroke(right, color: water,     width: 7.5)
            stroke(left,  color: water,     width: 7.5)
            stroke(right, color: midWater,  width: 4)
            stroke(left,  color: midWater,  width: 4)
            stroke(right, color: highlight, width: 1.5)
            stroke(left,  color: highlight, width: 1.5)

            // Foam dots at wave crests
            let foam = Color(red: 144/255, green: 224/255, blue: 239/255)
            ctx.fill(Path(ellipseIn: CGRect(x: 32, y: 10, width: 4, height: 4)), with: .color(foam))
            ctx.fill(Path(ellipseIn: CGRect(x: 12, y: 10, width: 4, height: 4)), with: .color(foam))

            // Centre node
            ctx.fill(Path(ellipseIn: CGRect(x: 20,   y: 20,   width: 8,   height: 8)),   with: .color(water))
            ctx.fill(Path(ellipseIn: CGRect(x: 21.8, y: 21.8, width: 4.4, height: 4.4)), with: .color(Color(red: 72/255, green: 202/255, blue: 228/255)))

            _ = roundCap
        }
        .aspectRatio(1, contentMode: .fit)
    }

    private var rightLobe: Path {
        var p = Path()
        p.move(to: .init(x: 24, y: 24))
        p.addCurve(to: .init(x: 38, y: 15), control1: .init(x: 28, y: 17), control2: .init(x: 34, y: 12))
        p.addCurve(to: .init(x: 44, y: 24), control1: .init(x: 42, y: 18), control2: .init(x: 44, y: 22))
        p.addCurve(to: .init(x: 38, y: 33), control1: .init(x: 44, y: 26), control2: .init(x: 42, y: 30))
        p.addCurve(to: .init(x: 24, y: 24), control1: .init(x: 34, y: 36), control2: .init(x: 28, y: 31))
        p.closeSubpath()
        return p
    }

    private var leftLobe: Path {
        var p = Path()
        p.move(to: .init(x: 24, y: 24))
        p.addCurve(to: .init(x: 10, y: 15), control1: .init(x: 20, y: 17), control2: .init(x: 14, y: 12))
        p.addCurve(to: .init(x:  4, y: 24), control1: .init(x:  6, y: 18), control2: .init(x:  4, y: 22))
        p.addCurve(to: .init(x: 10, y: 33), control1: .init(x:  4, y: 26), control2: .init(x:  6, y: 30))
        p.addCurve(to: .init(x: 24, y: 24), control1: .init(x: 14, y: 36), control2: .init(x: 20, y: 31))
        p.closeSubpath()
        return p
    }
}
