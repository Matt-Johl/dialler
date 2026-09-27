import DiallerCore
import SwiftUI

struct KeypadView: View {
    @EnvironmentObject private var model: AppModel
    @State private var number = ""
    /// Set by a long press on 0, which has already typed "+": the tap that
    /// ends the press must not type a 0 after it.
    @State private var plusTyped = false

    private static let keys: [[(digit: String, letters: String)]] = [
        [("1", ""), ("2", "ABC"), ("3", "DEF")],
        [("4", "GHI"), ("5", "JKL"), ("6", "MNO")],
        [("7", "PQRS"), ("8", "TUV"), ("9", "WXYZ")],
        [("*", ""), ("0", "+"), ("#", "")],
    ]

    /// The number of the last call made from this phone: as on the Phone
    /// app, Call with nothing typed brings it back to redial.
    private var lastDialled: String? {
        model.recents.first { $0.direction == .outgoing }?.number
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 5) {
                if !model.line.isEmpty {
                    Text("Calling from").foregroundStyle(Palette.secondary)
                    Text(CallText.party(model.line)).fontWeight(.medium).foregroundStyle(Palette.ink)
                }
            }
            .font(.footnote)
            .frame(height: 20)
            .padding(.top, 16)

            Spacer(minLength: 8)
            display
            Spacer(minLength: 8)

            Grid(horizontalSpacing: 28, verticalSpacing: 16) {
                ForEach(Self.keys.indices, id: \.self) { row in
                    GridRow {
                        ForEach(Self.keys[row], id: \.digit) { key in
                            keyButton(key.digit, key.letters)
                        }
                    }
                }
                GridRow {
                    Color.clear.frame(width: 76, height: 76)
                    callButton
                    deleteButton
                }
                .padding(.top, 4)
            }
            Spacer(minLength: 20)
        }
        .frame(maxWidth: .infinity)
        .background(Palette.ground)
    }

    private var display: some View {
        ZStack {
            if number.isEmpty {
                Text("Enter a number or extension")
                    .font(.title3.weight(.light))
                    .foregroundStyle(Palette.tertiary)
                    .multilineTextAlignment(.center)
            } else {
                Text(number)
                    .font(.system(size: 38, weight: .light))
                    .tracking(0.8)
                    .foregroundStyle(Palette.ink)
                    .lineLimit(1)
                    .minimumScaleFactor(0.5)
                    .textSelection(.enabled)
            }
        }
        .frame(maxWidth: .infinity, minHeight: 64)
        .padding(.horizontal, 24)
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.updatesFrequently)
    }

    private func keyButton(_ digit: String, _ letters: String) -> some View {
        Button {
            if plusTyped { plusTyped = false; return }
            number.append(digit)
        } label: {
            VStack(spacing: 1) {
                Text(digit)
                    .font(.system(size: digit == "*" ? 36 : 30, weight: .light))
                    .frame(height: 32)
                Text(letters)
                    .font(.system(size: 9.5, weight: .medium))
                    .tracking(2)
                    .foregroundStyle(Palette.secondary)
                    .frame(height: 12)
            }
        }
        .buttonStyle(CircleButtonStyle())
        .simultaneousGesture(LongPressGesture(minimumDuration: 0.5).onEnded { _ in
            guard digit == "0" else { return }
            number.append("+")
            plusTyped = true
        })
        .accessibilityLabel(letters.isEmpty || digit == "0" ? digit : "\(digit), \(letters)")
    }

    private var callButton: some View {
        Button {
            if number.isEmpty {
                number = lastDialled ?? ""
            } else {
                model.dial(number)
            }
        } label: {
            Image(systemName: "phone.fill").font(.system(size: 26))
        }
        .buttonStyle(CircleButtonStyle(on: true))
        .disabled(model.activeCall != nil || (number.isEmpty && lastDialled == nil))
        .accessibilityLabel("Call")
    }

    private var deleteButton: some View {
        Button {
            if !number.isEmpty { number.removeLast() }
        } label: {
            Image(systemName: "delete.left")
                .font(.system(size: 22, weight: .light))
                .foregroundStyle(Palette.secondary)
                .frame(width: 76, height: 76)
                .contentShape(Rectangle())
        }
        .opacity(number.isEmpty ? 0 : 1)
        .disabled(number.isEmpty)
        .simultaneousGesture(LongPressGesture().onEnded { _ in number = "" })
        .accessibilityLabel("Delete")
    }
}
