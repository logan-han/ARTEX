"use client";

import { useEffect, useRef, useState } from "react";

import { useRouter } from "next/navigation";

import { AlertTriangle, ShieldCheck } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogClose, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { auth } from "@/lib/auth";

export default function LoginPage() {
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(true);
  const [agreed, setAgreed] = useState(false);
  const [termsOpen, setTermsOpen] = useState(false);
  const [readToEnd, setReadToEnd] = useState(false);
  const termsBodyRef = useRef<HTMLDivElement>(null);

  // The "Agree" button is enabled only after scrolling to the bottom of the terms (including when they fit without scrolling).
  function handleTermsScroll() {
    const el = termsBodyRef.current;
    if (!el) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 8) setReadToEnd(true);
  }

  useEffect(() => {
    if (!termsOpen) return;
    // Reset on open, and handle content that is already shorter than one screen and so can never fire a scroll event.
    setReadToEnd(false);
    const el = termsBodyRef.current;
    if (el && el.scrollHeight <= el.clientHeight + 8) setReadToEnd(true);
  }, [termsOpen]);

  useEffect(() => {
    // Already logged in: go straight to the main UI (with static export there is no middleware to do this redirect).
    const token = auth.getToken();
    if (token) {
      // localStorage may still hold credentials while the cookie is gone. Sync first, then make a fresh request,
      // so the server-side guard or router cache does not send the redirect back to a login page still in the checking state.
      auth.setToken(token);
      window.location.replace("/function/tasks");
      return;
    }
    api
      .authStatus()
      .then(({ initialized }) => {
        if (!initialized) router.replace("/setup");
      })
      .catch(() => setError("Cannot connect to the backend service"))
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!agreed) {
      setError("Please read and agree to the terms of use first");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.login("ARTEX", password);
      auth.setToken(token);
      window.location.replace("/function/tasks");
    } catch {
      setError("Incorrect username or password");
    } finally {
      setLoading(false);
    }
  }

  if (checking) {
    return (
      <div role="status" className="flex min-h-dvh items-center justify-center text-muted-foreground">
        Checking login status...
      </div>
    );
  }

  return (
    <div className="flex h-dvh">
      {/* Left panel */}
      <div className="hidden flex-col items-center justify-center bg-primary p-12 text-center lg:flex lg:w-1/3">
        <div className="relative flex items-center justify-center">
          <div className="absolute size-80 rounded-full border border-primary-foreground/10" />
          <div className="absolute size-60 rounded-full border border-primary-foreground/15" />
          <div className="absolute size-40 rounded-full border border-primary-foreground/20" />
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.png" alt="ARTEX" width={160} height={160} className="relative brightness-0 invert" />
        </div>
      </div>

      {/* Right panel */}
      <div className="flex w-full items-center justify-center bg-background p-8 lg:w-2/3">
        <div className="w-full max-w-md space-y-10 py-24 lg:py-32">
          <div className="space-y-4 text-center">
            <h2 className="text-2xl font-medium tracking-tight">Log in</h2>
            <p className="mx-auto max-w-xl text-muted-foreground">Welcome back, enter your password to continue using ARTEX</p>
          </div>
          <form onSubmit={handleSubmit} className="flex flex-col gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="username">Username</Label>
              <Input id="username" value="ARTEX" readOnly className="bg-muted text-muted-foreground" />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="password">Password</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="Enter your password"
                autoFocus
                autoComplete="current-password"
              />
            </div>
            <div className="flex items-start gap-2">
              <Checkbox
                id="agree-terms"
                checked={agreed}
                onCheckedChange={(v) => setAgreed(v === true)}
                className="mt-0.5"
              />
              <Label htmlFor="agree-terms" className="text-sm font-normal leading-relaxed text-muted-foreground">
                I have read and agree to the
                <button
                  type="button"
                  onClick={() => setTermsOpen(true)}
                  className="mx-0.5 font-medium text-primary underline-offset-4 hover:underline"
                >
                  terms of use
                </button>
              </Label>
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button type="submit" className="w-full" disabled={loading || !password || !agreed}>
              {loading ? "Logging in..." : "Log in"}
            </Button>
          </form>
        </div>
      </div>

      <Dialog open={termsOpen} onOpenChange={setTermsOpen}>
        <DialogContent className="gap-0 p-0 sm:max-w-2xl">
          <DialogHeader className="flex-row items-center gap-3 border-b px-6 py-4">
            <div className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <ShieldCheck className="size-5" />
            </div>
            <div className="space-y-0.5">
              <DialogTitle className="text-base">ARTEX terms of use and disclaimer</DialogTitle>
              <p className="text-xs text-muted-foreground">
                Version v1.0 · Effective 2026-09-18 · Read all of the terms below in full before logging in
              </p>
            </div>
          </DialogHeader>

          <div
            ref={termsBodyRef}
            onScroll={handleTermsScroll}
            className="max-h-[60vh] space-y-5 overflow-y-auto px-6 py-5 text-sm leading-relaxed text-muted-foreground"
          >
            <p className="rounded-lg border bg-muted/40 p-3 text-foreground/80">
              These terms of use and disclaimer (hereinafter "this Statement") are an agreement between you and the authors and contributors of the ARTEX project regarding your use of this software. Read them carefully before use and make sure you fully understand each term, especially the disclaimer, limitation of liability and prohibition terms marked in bold or highlighted blocks.
              <span className="font-medium text-foreground">
                {" "}
                By downloading, installing, accessing or using this software in any way, you are deemed to have read, understood and agreed to be bound by all of this Statement.
              </span>
            </p>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  1
                </span>
                Article 1 · Definitions and open source licence
              </h4>
              <p className="pl-7">
                This software (ARTEX) is an open source program released under the GNU Affero General Public License v3.0 (AGPL-3.0). You may freely use, copy, modify and distribute this software under that licence; however, any derivative work (including an online service provided to third parties over a network) must likewise be open sourced under the AGPL-3.0, with the corresponding complete source code made available to its users. The full AGPL-3.0 terms are as set out in the accompanying LICENSE file.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  2
                </span>
                Article 2 · Scope of permitted use
              </h4>
              <p className="pl-7">
                This software is provided only for personal learning, code research, discussion of security technology principles, and technical validation in an isolated local environment that you set up yourself; it is intended for non-offensive, non-destructive uses such as learning, academic research and code review. Except as expressly permitted in this article, you must not use this software for any other purpose.
              </p>
            </section>

            <section className="space-y-2">
              <h4 className="flex items-center gap-2 font-medium text-destructive">
                <span className="flex size-5 items-center justify-center rounded-md bg-destructive/10 text-xs font-semibold text-destructive">
                  3
                </span>
                <AlertTriangle className="size-4" />
                Article 3 · Prohibited conduct
              </h4>
              <ul className="ml-7 list-decimal space-y-1.5 rounded-lg border border-destructive/20 bg-destructive/5 p-3 pl-8 text-foreground/80 marker:text-destructive/70">
                <li>
                  You are strictly prohibited from scanning, probing, exploiting or attacking any website, online service, or networked system owned by others or third parties (regardless of whether authorisation has been obtained or whether it is your own asset);
                </li>
                <li>You are strictly prohibited from using this software for any real penetration testing, attack and defence exercises, red/blue team drills or production environments;</li>
                <li>You are strictly prohibited from using this software for unlawful intrusion, data theft, extortion, denial of service (DoS/DDoS) or any destructive or criminal activity;</li>
                <li>You are strictly prohibited from removing, tampering with or circumventing any copyright, licence or security notice in this software or its output;</li>
                <li>You are strictly prohibited from any conduct that violates the laws, regulations or regulatory requirements of your country or region.</li>
              </ul>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  4
                </span>
                Article 4 · Intellectual property
              </h4>
              <p className="pl-7">
                Copyright and related intellectual property rights in this software belong to the project's authors and contributors, who grant you the corresponding rights within the scope set out in the AGPL-3.0. Apart from the rights expressly granted by that licence, this Statement does not grant you any other rights, express or implied.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  5
                </span>
                Article 5 · Data and privacy
              </h4>
              <p className="pl-7">
                This software is a self-hosted open source program; the authors do not operate any centralised service and do not collect or upload your usage data. All data you generate, process or come into contact with while using it is under your own control, and you are responsible for its lawfulness and security; you bear any consequences arising from improper handling of data.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  6
                </span>
                Article 6 · Compliance and legal liability
              </h4>
              <p className="pl-7">
                You must comply on your own with all laws and regulations of your country or region on cybersecurity, data security and personal information protection, computer crime and related matters (in mainland China, including but not limited to the Cybersecurity Law, the Data Security Law, the Personal Information Protection Law and the related judicial interpretations).
                <span className="font-medium text-foreground">
                  {" "}
                  All legal liability and consequences arising from your breach of the above laws and regulations or of this Statement are borne solely by you and have nothing to do with the authors and contributors of this software.
                </span>
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  7
                </span>
                Article 7 · Disclaimer and limitation of liability
              </h4>
              <p className="pl-7">
                This software is provided "AS IS" and "AS AVAILABLE", without any warranty, express or implied, including but not limited to warranties of merchantability, fitness for a particular purpose, accuracy and non-infringement. To the maximum extent permitted by applicable law, the authors and contributors of this software are not liable for any direct, indirect, incidental, special or consequential loss resulting from the use of or inability to use this software (whether or not it was used properly), including but not limited to data loss, system damage, business interruption, loss of profits or legal disputes.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  8
                </span>
                Article 8 · Changes to the terms and final interpretation
              </h4>
              <p className="pl-7">
                The authors may update this Statement from time to time as required by laws and regulations or by the project's development; the updated version is released with the project and takes effect from the date of publication, and your continued use of this software means you accept the revised terms. To the extent permitted by law, the right of final interpretation of this Statement rests with the project's authors. If any term of this Statement is held invalid, the validity of the remaining terms is not affected.
              </p>
            </section>
          </div>

          <DialogFooter className="mx-0 mb-0 flex-col items-stretch gap-2 rounded-b-xl px-6 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-xs text-muted-foreground">
              {readToEnd ? "You have read all of the terms" : "Scroll to the bottom of the terms before confirming"}
            </p>
            <DialogClose asChild>
              <Button
                type="button"
                disabled={!readToEnd}
                onClick={() => {
                  setAgreed(true);
                  setError("");
                }}
              >
                I have read and agree to all of the terms
              </Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
