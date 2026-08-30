import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Relay Console",
  description:
    "Explain a routing decision for free, then run one for real and see what it saved.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
