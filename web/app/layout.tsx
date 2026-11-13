import "./globals.css";

export const metadata = { title: "Anton Platform", description: "Durable AI agent sessions on Kubernetes" };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
