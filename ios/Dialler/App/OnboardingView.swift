import DiallerCore
import SwiftUI
import VisionKit

/// First run (SPEC §6 item 8): the app has no credential, so it asks for
/// one — by scanning the QR the administrator shows, or by typing the
/// server and the code. Both end in `AppModel.enrol`, which claims the
/// code, pins the server's certificate and connects.
struct OnboardingView: View {
    @EnvironmentObject private var model: AppModel
    @State private var showScanner = false
    @State private var manual = false
    /// The hidden Status page before enrolment: the dev path in (SPEC §6
    /// item 8), behind the same five-second press as the Settings title.
    @State private var showStatus = false
    @State private var host = ""
    @State private var port = "8080"
    @State private var code = ""

    var body: some View {
        NavigationStack {
            VStack(spacing: 28) {
                Spacer()
                Image(systemName: "phone.badge.checkmark")
                    .font(.system(size: 64))
                    .foregroundStyle(.tint)
                    .contentShape(Rectangle())
                    .onLongPressGesture(minimumDuration: 5) { showStatus = true }
                Text("Set up this phone")
                    .font(.largeTitle.weight(.semibold))
                Text("Your administrator has an enrolment code for this phone, shown as a QR code or as eight characters.")
                    .multilineTextAlignment(.center)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal)
                if model.enrolling {
                    ProgressView("Enrolling…")
                } else {
                    VStack(spacing: 12) {
                        Button {
                            showScanner = true
                        } label: {
                            Label("Scan QR code", systemImage: "qrcode.viewfinder").frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.borderedProminent)
                        .controlSize(.large)
                        .disabled(!QRScannerView.isAvailable)
                        Button {
                            manual = true
                        } label: {
                            Label("Enter details manually", systemImage: "keyboard").frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.bordered)
                        .controlSize(.large)
                    }
                    .padding(.horizontal, 32)
                    if !QRScannerView.isAvailable {
                        Text("No camera here: enter the details instead.").font(.caption).foregroundStyle(.secondary)
                    }
                }
                if let err = model.enrolmentError {
                    Text(err).font(.callout).foregroundStyle(.red).multilineTextAlignment(.center).padding(.horizontal)
                }
                Spacer()
                Spacer()
            }
            .padding()
            .sheet(isPresented: $showScanner) {
                QRScannerView { url in
                    showScanner = false
                    Task { await model.enrol(url: url) }
                }
            }
            .sheet(isPresented: $manual) { manualForm }
            .sheet(isPresented: $showStatus) {
                NavigationStack {
                    StatusView()
                        .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { showStatus = false } } }
                }
            }
        }
    }

    private var manualForm: some View {
        NavigationStack {
            Form {
                Section("Server") {
                    TextField("Address (name or IP)", text: $host)
                        .textInputAutocapitalization(.never).autocorrectionDisabled().keyboardType(.URL)
                    TextField("Port", text: $port).keyboardType(.numberPad)
                }
                Section("Enrolment code") {
                    TextField("8 characters", text: $code)
                        .textInputAutocapitalization(.characters).autocorrectionDisabled()
                        .font(.body.monospaced())
                }
                Section {
                    Button("Enrol") {
                        manual = false
                        Task { await model.enrol(host: host, port: UInt16(port) ?? 8080, code: code) }
                    }
                    .disabled(host.trimmingCharacters(in: .whitespaces).isEmpty || EnrolmentLink.normalise(code).count < 8)
                }
            }
            .navigationTitle("Enter details")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button("Cancel") { manual = false } } }
        }
    }
}

/// VisionKit's scanner, looking for one `dialler://enrol` QR. Not available
/// on the simulator or without a camera; the caller offers manual entry.
struct QRScannerView: UIViewControllerRepresentable {
    let found: (URL) -> Void

    static var isAvailable: Bool {
        DataScannerViewController.isSupported && DataScannerViewController.isAvailable
    }

    func makeUIViewController(context: Context) -> DataScannerViewController {
        let vc = DataScannerViewController(recognizedDataTypes: [.barcode(symbologies: [.qr])],
                                           qualityLevel: .balanced, recognizesMultipleItems: false,
                                           isHighFrameRateTrackingEnabled: false, isHighlightingEnabled: true)
        vc.delegate = context.coordinator
        try? vc.startScanning()
        return vc
    }

    func updateUIViewController(_ uiViewController: DataScannerViewController, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(found: found) }

    final class Coordinator: NSObject, DataScannerViewControllerDelegate {
        let found: (URL) -> Void
        private var done = false
        init(found: @escaping (URL) -> Void) { self.found = found }

        func dataScanner(_ dataScanner: DataScannerViewController, didAdd addedItems: [RecognizedItem], allItems: [RecognizedItem]) {
            guard !done else { return }
            for item in addedItems {
                if case .barcode(let b) = item, let s = b.payloadStringValue, let url = URL(string: s), EnrolmentLink(url: url) != nil {
                    done = true
                    dataScanner.stopScanning()
                    found(url)
                    return
                }
            }
        }
    }
}
